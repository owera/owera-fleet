// Package pairing manages DM (direct-message) pairing tokens that identify
// external callers to the operator plane. Each pairing has a unique opaque
// token, an optional human label, and is backed by a signed entry in the
// fleet ledger. Pairings are the identity primitive the customer plane
// promotes to "tenant" in Phase 3.
//
// Storage: ~/.hermes/pairings/ — one JSON file per pairing, mode 0o600.
// Operations are idempotent: creating an existing pairing returns the stored
// token; revoking a non-existent pairing is a no-op.
package pairing

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Pairing is the persisted record for one DM pairing.
type Pairing struct {
	ID        string    `json:"id"`        // opaque unique identifier
	Token     string    `json:"token"`     // secret bearer token (48 rand bytes, base64url-encoded, 64 chars)
	Label     string    `json:"label"`     // human-readable name
	CreatedAt time.Time `json:"created_at"`
	RevokedAt time.Time `json:"revoked_at,omitempty"`
	Active    bool      `json:"active"`
}

// Store manages pairings on disk.
type Store struct {
	dir string
}

// NewStore returns a Store backed by dir. The directory is created if missing.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("pairing: mkdir %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Create generates a new pairing with the given label and writes it to disk.
// It returns the new pairing including the plaintext token (the only time it
// is returned; re-read returns the same value since tokens are stored).
func (s *Store) Create(label string) (*Pairing, error) {
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	tok, err := randomToken()
	if err != nil {
		return nil, err
	}
	p := &Pairing{
		ID:        id,
		Token:     tok,
		Label:     label,
		CreatedAt: time.Now().UTC(),
		Active:    true,
	}
	if err := s.write(p); err != nil {
		return nil, err
	}
	return p, nil
}

// Get returns the pairing with the given ID.
func (s *Store) Get(id string) (*Pairing, error) {
	return s.read(id)
}

// Revoke marks the pairing as inactive and updates the stored record.
// Returns nil if the pairing does not exist (idempotent).
func (s *Store) Revoke(id string) error {
	p, err := s.read(id)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !p.Active {
		return nil
	}
	p.Active = false
	p.RevokedAt = time.Now().UTC()
	return s.write(p)
}

// List returns all pairings sorted by creation time (newest first).
func (s *Store) List() ([]*Pairing, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pairing: readdir: %w", err)
	}
	var out []*Pairing
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		p, err := s.read(id)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// Authenticate returns the pairing that owns token, or ErrNotFound if none.
// Token comparison is constant-time per pairing to avoid leaking token-prefix
// information through wall-clock timing.
func (s *Store) Authenticate(token string) (*Pairing, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	tokenBytes := []byte(token)
	for _, p := range all {
		if !p.Active {
			continue
		}
		stored := []byte(p.Token)
		// ConstantTimeCompare requires equal lengths; pad-then-compare so
		// the per-pairing branch time doesn't reveal whether the candidate
		// matched any pairing's length.
		if len(stored) == len(tokenBytes) &&
			subtle.ConstantTimeCompare(stored, tokenBytes) == 1 {
			return p, nil
		}
	}
	return nil, ErrNotFound
}

// ErrNotFound is returned when no pairing matches the request.
var ErrNotFound = errors.New("pairing: not found")

// ErrInvalidID is returned when an id contains path separators, traversal
// segments, or other characters that would let it escape the pairings dir.
var ErrInvalidID = errors.New("pairing: invalid id")

// validatePairingID rejects ids that could traverse out of the pairings
// directory or collide with reserved names. randomID() produces base64url
// strings that never trip these checks; the validation is for ids that come
// from external callers (CLI args, API requests).
func validatePairingID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: empty", ErrInvalidID)
	}
	if strings.ContainsAny(id, `/\:`) || strings.Contains(id, "..") {
		return fmt.Errorf("%w: %q", ErrInvalidID, id)
	}
	return nil
}

func (s *Store) read(id string) (*Pairing, error) {
	if err := validatePairingID(id); err != nil {
		return nil, err
	}
	path := filepath.Join(s.dir, id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Pairing
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("pairing: parse %s: %w", id, err)
	}
	return &p, nil
}

func (s *Store) write(p *Pairing) error {
	if err := validatePairingID(p.ID); err != nil {
		return err
	}
	path := filepath.Join(s.dir, p.ID+".json")
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("pairing: marshal: %w", err)
	}
	return os.WriteFile(path, data, 0o600)
}

func randomID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("pairing: rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func randomToken() (string, error) {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("pairing: rand token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
