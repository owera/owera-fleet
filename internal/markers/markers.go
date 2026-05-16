// Package markers provides content-hashed marker files for idempotent
// step execution in ETL-style pipelines. A marker records that a named step
// completed with a particular input hash; re-running the step with the same
// input is a no-op. If the input changes, the marker is stale and the step
// must re-run.
//
// Markers live under ~/.hermes/markers/<pipeline>/<step>.json and contain:
//
//	{
//	  "step": "load_users",
//	  "input_hash": "sha256:...",
//	  "completed_at": "2026-05-16T...",
//	  "output_path": "/tmp/users.parquet",
//	  "duration_ms": 12345
//	}
//
// The freshness check (is this marker still valid?) hashes the current input
// and compares; if equal, the step is skipped.
package markers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Marker is one step completion record.
type Marker struct {
	Pipeline    string    `json:"pipeline"`
	Step        string    `json:"step"`
	InputHash   string    `json:"input_hash"`
	CompletedAt time.Time `json:"completed_at"`
	OutputPath  string    `json:"output_path,omitempty"`
	DurationMs  int64     `json:"duration_ms,omitempty"`
}

// Store manages marker files rooted at a directory.
type Store struct {
	dir string
}

// NewStore returns a Store backed by dir, creating it if necessary.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("markers: mkdir %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Write persists a marker. The Pipeline and Step fields are used as the
// storage key; if they are empty, an error is returned.
func (s *Store) Write(m Marker) error {
	if m.Pipeline == "" || m.Step == "" {
		return errors.New("markers: pipeline and step are required")
	}
	if m.CompletedAt.IsZero() {
		m.CompletedAt = time.Now().UTC()
	}
	path := s.path(m.Pipeline, m.Step)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("markers: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("markers: marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("markers: write: %w", err)
	}
	return os.Rename(tmp, path)
}

// Read returns the stored marker for (pipeline, step), or ErrNotFound.
func (s *Store) Read(pipeline, step string) (*Marker, error) {
	data, err := os.ReadFile(s.path(pipeline, step))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("markers: read: %w", err)
	}
	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("markers: parse: %w", err)
	}
	return &m, nil
}

// IsFresh returns true if a marker exists for (pipeline, step) and its
// InputHash matches the supplied hash.
func (s *Store) IsFresh(pipeline, step, inputHash string) (bool, error) {
	m, err := s.Read(pipeline, step)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return m.InputHash == inputHash, nil
}

// Invalidate removes a marker, forcing the next IsFresh check to return false.
// Returns nil if the marker did not exist.
func (s *Store) Invalidate(pipeline, step string) error {
	err := os.Remove(s.path(pipeline, step))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// List returns all markers for a pipeline sorted by step name.
func (s *Store) List(pipeline string) ([]*Marker, error) {
	dir := filepath.Join(s.dir, pipeline)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("markers: list: %w", err)
	}
	var out []*Marker
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		step := strings.TrimSuffix(e.Name(), ".json")
		m, err := s.Read(pipeline, step)
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Step < out[j].Step })
	return out, nil
}

// Pipelines returns the set of pipeline directories under the store.
func (s *Store) Pipelines() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("markers: pipelines: %w", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// ErrNotFound is returned when no marker exists for the requested key.
var ErrNotFound = errors.New("markers: not found")

// HashInput returns the canonical sha256 hash of an input value, formatted as
// "sha256:<hex>". Inputs can be a string, []byte, or io.Reader.
func HashInput(input any) (string, error) {
	h := sha256.New()
	switch v := input.(type) {
	case string:
		h.Write([]byte(v))
	case []byte:
		h.Write(v)
	case io.Reader:
		if _, err := io.Copy(h, v); err != nil {
			return "", fmt.Errorf("markers: hash: %w", err)
		}
	default:
		return "", fmt.Errorf("markers: unsupported input type %T", input)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// HashFile returns the sha256 of a file's contents.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("markers: open %s: %w", path, err)
	}
	defer f.Close()
	return HashInput(io.Reader(f))
}

func (s *Store) path(pipeline, step string) string {
	return filepath.Join(s.dir, pipeline, step+".json")
}
