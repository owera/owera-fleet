package budget_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/owera/owera-fleet/internal/budget"
)

func TestConsumeAndGet(t *testing.T) {
	s, err := budget.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.Consume("p1", 100); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	st, err := s.Get("p1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if st.Requests != 1 {
		t.Errorf("Requests = %d, want 1", st.Requests)
	}
	if st.CostCents != 100 {
		t.Errorf("CostCents = %d, want 100", st.CostCents)
	}
}

func TestRateLimitExceeded(t *testing.T) {
	s, _ := budget.NewStore(t.TempDir())
	_ = s.SetLimits("p2", 2, 100_000)

	_ = s.Consume("p2", 0)
	_ = s.Consume("p2", 0)

	err := s.Consume("p2", 0)
	if !errors.Is(err, budget.ErrRateLimitExceeded) {
		t.Errorf("expected ErrRateLimitExceeded, got %v", err)
	}
}

func TestCostCapExceeded(t *testing.T) {
	s, _ := budget.NewStore(t.TempDir())
	_ = s.SetLimits("p3", 1000, 100) // $1.00 cap

	_ = s.Consume("p3", 60)    // $0.60
	err := s.Consume("p3", 50) // would be $1.10
	if !errors.Is(err, budget.ErrCostCapExceeded) {
		t.Errorf("expected ErrCostCapExceeded, got %v", err)
	}
}

func TestCheck(t *testing.T) {
	s, _ := budget.NewStore(t.TempDir())
	_ = s.SetLimits("p4", 1, 1000)
	_ = s.Consume("p4", 0) // uses the 1 request

	if err := s.Check("p4", 0); !errors.Is(err, budget.ErrRateLimitExceeded) {
		t.Errorf("Check should see limit exceeded: %v", err)
	}
}

func TestDefaultLimits(t *testing.T) {
	s, _ := budget.NewStore(t.TempDir())
	st, err := s.Get("new-pairing")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if st.MaxRequests != budget.DefaultDailyRequests {
		t.Errorf("MaxRequests = %d", st.MaxRequests)
	}
	if st.MaxCostCents != budget.DefaultDailyCostCents {
		t.Errorf("MaxCostCents = %d", st.MaxCostCents)
	}
}

// TestConsumeConcurrentRespectsCap — N goroutines race to Consume against a
// cap of M (M < N). Pre-fix (no flock around the read-modify-write) this
// races: multiple goroutines pass the Requests<MaxRequests check, both
// commit, and the final Requests count exceeds MaxRequests. Post-fix, the
// per-pairing flock serializes Consume, so exactly M succeed and N-M fail
// with ErrRateLimitExceeded.
func TestConsumeConcurrentRespectsCap(t *testing.T) {
	const N = 32
	const M = 10
	s, err := budget.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.SetLimits("race", M, 100_000); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}

	var ok, rejected int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.Consume("race", 1)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else if errors.Is(err, budget.ErrRateLimitExceeded) {
				rejected++
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if ok != M {
		t.Errorf("ok = %d, want %d (cap exceeded under contention — flock not serializing)", ok, M)
	}
	if rejected != N-M {
		t.Errorf("rejected = %d, want %d", rejected, N-M)
	}
	st, err := s.Get("race")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if st.Requests != M {
		t.Errorf("final Requests = %d, want %d (state corruption under contention)", st.Requests, M)
	}
}
