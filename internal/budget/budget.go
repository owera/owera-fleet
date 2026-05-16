// Package budget enforces per-pairing rate limits and cost caps. Each pairing
// has a daily request count and an accumulated cost (in USD cents) tracked in
// a JSON file under ~/.hermes/budgets/. Checks and increments are serialized
// under a file lock via rename-on-write so concurrent fleetctl invocations
// don't corrupt the budget state.
package budget

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultDailyRequests is the request ceiling per pairing per calendar day.
const DefaultDailyRequests = 1000

// DefaultDailyCostCents is the default cost cap per pairing per calendar day
// (in USD cents). 10_000 = $100.
const DefaultDailyCostCents = 10_000

// State holds the rolling budget state for one pairing.
type State struct {
	PairingID    string    `json:"pairing_id"`
	Date         string    `json:"date"` // YYYY-MM-DD
	Requests     int       `json:"requests"`
	CostCents    int64     `json:"cost_cents"`
	LastUpdated  time.Time `json:"last_updated"`
	MaxRequests  int       `json:"max_requests"`
	MaxCostCents int64     `json:"max_cost_cents"`
}

// ErrRateLimitExceeded is returned when a pairing has hit its daily request limit.
var ErrRateLimitExceeded = errors.New("budget: daily request limit exceeded")

// ErrCostCapExceeded is returned when a pairing has hit its daily cost cap.
var ErrCostCapExceeded = errors.New("budget: daily cost cap exceeded")

// Store manages budget state on disk.
type Store struct {
	dir string
}

// NewStore returns a Store backed by dir, creating it if necessary.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("budget: mkdir %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Check returns ErrRateLimitExceeded or ErrCostCapExceeded if the pairing
// cannot accept another request at costCents. Returns nil if the request
// is within budget.
func (s *Store) Check(pairingID string, costCents int64) error {
	st, err := s.load(pairingID)
	if err != nil {
		return err
	}
	st = rollDay(st)
	if st.Requests >= st.MaxRequests {
		return fmt.Errorf("%w: %s has %d/%d requests today", ErrRateLimitExceeded, pairingID, st.Requests, st.MaxRequests)
	}
	if st.CostCents+costCents > st.MaxCostCents {
		return fmt.Errorf("%w: %s has $%d.%02d/$%d.%02d spent today",
			ErrCostCapExceeded, pairingID,
			st.CostCents/100, st.CostCents%100,
			st.MaxCostCents/100, st.MaxCostCents%100,
		)
	}
	return nil
}

// Consume checks and, if within budget, increments the counters atomically.
func (s *Store) Consume(pairingID string, costCents int64) error {
	st, err := s.load(pairingID)
	if err != nil {
		return err
	}
	st = rollDay(st)
	if st.Requests >= st.MaxRequests {
		return fmt.Errorf("%w: %s", ErrRateLimitExceeded, pairingID)
	}
	if st.CostCents+costCents > st.MaxCostCents {
		return fmt.Errorf("%w: %s", ErrCostCapExceeded, pairingID)
	}
	st.Requests++
	st.CostCents += costCents
	st.LastUpdated = time.Now().UTC()
	return s.save(st)
}

// Get returns the current budget state for a pairing, rolling the day if
// needed (so Requests/CostCents are for today, not yesterday).
func (s *Store) Get(pairingID string) (*State, error) {
	st, err := s.load(pairingID)
	if err != nil {
		return nil, err
	}
	rolled := rollDay(st)
	return &rolled, nil
}

// SetLimits overwrites the per-pairing daily limits. A maxRequests or
// maxCostCents of 0 keeps the current default.
func (s *Store) SetLimits(pairingID string, maxRequests int, maxCostCents int64) error {
	st, err := s.load(pairingID)
	if err != nil {
		return err
	}
	if maxRequests > 0 {
		st.MaxRequests = maxRequests
	}
	if maxCostCents > 0 {
		st.MaxCostCents = maxCostCents
	}
	return s.save(st)
}

func (s *Store) path(pairingID string) string {
	return filepath.Join(s.dir, pairingID+".json")
}

func (s *Store) load(pairingID string) (State, error) {
	data, err := os.ReadFile(s.path(pairingID))
	if errors.Is(err, os.ErrNotExist) {
		return State{
			PairingID:    pairingID,
			Date:         todayStr(),
			MaxRequests:  DefaultDailyRequests,
			MaxCostCents: DefaultDailyCostCents,
		}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("budget: read %s: %w", pairingID, err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fmt.Errorf("budget: parse %s: %w", pairingID, err)
	}
	return st, nil
}

func (s *Store) save(st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("budget: marshal: %w", err)
	}
	// Write to a temp file then rename for atomic update.
	tmp := s.path(st.PairingID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("budget: write: %w", err)
	}
	return os.Rename(tmp, s.path(st.PairingID))
}

// rollDay resets counters if the stored date is not today.
func rollDay(st State) State {
	today := todayStr()
	if st.Date == today {
		return st
	}
	st.Date = today
	st.Requests = 0
	st.CostCents = 0
	return st
}

func todayStr() string {
	return time.Now().UTC().Format("2006-01-02")
}
