// Package budget enforces per-pairing rate limits and cost caps. Each pairing
// has a daily request count and an accumulated cost (in USD cents) tracked in
// a JSON file under ~/.hermes/budgets/. Atomic Check+Consume operations
// serialize under a per-pairing advisory flock on <pairingID>.lock so that
// concurrent fleetctl invocations cannot exceed the daily caps. Writes are
// crash-safe via temp-file + fsync + rename + dir-fsync.
package budget

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
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
// cannot accept another request at costCents. Check holds the per-pairing
// lock during its read, so the snapshot is coherent — but the lock is
// released before Check returns. Callers MUST NOT rely on Check + Consume
// from separate calls being atomic; use Consume, which folds the check and
// the increment under a single lock acquisition.
func (s *Store) Check(pairingID string, costCents int64) error {
	return s.withLock(pairingID, func() error {
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
	})
}

// Consume atomically checks and, if within budget, increments the counters.
// Concurrent Consume calls for the same pairing serialize through a per-
// pairing flock so the daily caps are never exceeded by a TOCTOU race.
func (s *Store) Consume(pairingID string, costCents int64) error {
	return s.withLock(pairingID, func() error {
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
	})
}

// Get returns the current budget state for a pairing, rolling the day if
// needed (so Requests/CostCents are for today, not yesterday).
func (s *Store) Get(pairingID string) (*State, error) {
	var rolled State
	err := s.withLock(pairingID, func() error {
		st, err := s.load(pairingID)
		if err != nil {
			return err
		}
		rolled = rollDay(st)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &rolled, nil
}

// SetLimits overwrites the per-pairing daily limits. A maxRequests or
// maxCostCents of 0 keeps the current default.
func (s *Store) SetLimits(pairingID string, maxRequests int, maxCostCents int64) error {
	return s.withLock(pairingID, func() error {
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
	})
}

// withLock acquires an exclusive flock on <pairingID>.lock for the duration
// of f. The lock file is never renamed, so the flock holds across the
// rename-on-write that save() performs against <pairingID>.json. Different
// pairings use different lock files and do not block each other.
func (s *Store) withLock(pairingID string, f func() error) error {
	lockPath := filepath.Join(s.dir, pairingID+".lock")
	fh, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("budget: open lock %s: %w", lockPath, err)
	}
	defer fh.Close()
	if err := syscall.Flock(int(fh.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("budget: flock %s: %w", lockPath, err)
	}
	defer func() { _ = syscall.Flock(int(fh.Fd()), syscall.LOCK_UN) }()
	return f()
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
	// Crash-safe write: tmp file → fsync tmp → rename → fsync dir.
	// Without fsync, a crash between the rename and the kernel writeback
	// can revert the counter, allowing a billed request to be billed
	// twice on restart.
	path := s.path(st.PairingID)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("budget: open tmp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("budget: write tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("budget: fsync tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("budget: close tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("budget: rename: %w", err)
	}
	if dirF, derr := os.Open(s.dir); derr == nil {
		_ = dirF.Sync()
		_ = dirF.Close()
	}
	return nil
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
