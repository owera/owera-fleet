// Package orchestrator coordinates a parent agent + N leaf-subagent swarm.
//
// A swarm dispatches per-leaf tasks across multiple worker nodes, collects
// the per-worker ledger entries, and merges them into a single signed ledger
// for the parent task-id. Failures of individual leaves trigger retry or
// compensation (caller-provided) rather than aborting the whole campaign.
//
// The orchestrator does not itself dial SSH or invoke Hermes — those are
// injected through the [LeafRunner] type so the package stays focused on the
// scheduling + ledger-merge logic and tests don't need a live fleet.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/owera/owera-fleet/internal/ledger"
)

// LeafInput is the input to one leaf invocation.
type LeafInput struct {
	Node    string
	TaskID  string         // parent task ID; leaves all share it
	LeafID  string         // unique within the swarm
	Payload map[string]any // arbitrary leaf-specific data
}

// LeafResult is the outcome of one leaf invocation.
type LeafResult struct {
	LeafID   string
	Node     string
	Entries  []ledger.Entry // per-leaf ledger entries to merge into the parent ledger
	Err      error
	Duration time.Duration
}

// LeafRunner executes a single leaf assignment and returns its ledger entries.
// Implementations are responsible for emitting their own per-node JSONL log
// entries as appropriate; the orchestrator only consumes the returned ledger
// entries for merging.
type LeafRunner func(ctx context.Context, in LeafInput) ([]ledger.Entry, error)

// Plan describes the work for one swarm run.
//
// TenantID, when non-empty, is stamped onto every ledger entry the
// orchestrator writes for this plan (the parent swarm markers as well as
// every merged leaf entry). It propagates the customer-plane tenant identity
// from `fleet.SubmitJob` (see issue #5) down to the lowest-level billing
// rows, so the customer plane can reconcile against Stripe usage records
// without joining on task ID. Operator-only swarms leave it empty.
type Plan struct {
	TaskID    string
	TenantID  string
	ParentRun string // free-form description, e.g. "campaign-swarm v2"
	Leaves    []LeafInput
	MaxParallel int // 0 = unbounded
	RetryEach   int // re-run a failing leaf up to RetryEach times (0 = no retry)
}

// SwarmResult aggregates the per-leaf outcomes.
type SwarmResult struct {
	TaskID    string
	Leaves    []LeafResult
	OK        bool
	StartedAt time.Time
	EndedAt   time.Time
}

// Orchestrator drives a swarm and merges per-leaf ledger entries into the
// parent task's signed ledger.
type Orchestrator struct {
	Ledger *ledger.Ledger
	Run    LeafRunner
}

// Execute runs the swarm. Each leaf is executed (possibly in parallel up to
// plan.MaxParallel), and on success its returned entries are appended to the
// parent ledger under plan.TaskID. The aggregate SwarmResult is returned.
func (o *Orchestrator) Execute(ctx context.Context, plan Plan) (*SwarmResult, error) {
	if o.Run == nil {
		return nil, errors.New("orchestrator: LeafRunner is nil")
	}
	if plan.TaskID == "" {
		return nil, errors.New("orchestrator: plan.TaskID is required")
	}

	result := &SwarmResult{
		TaskID:    plan.TaskID,
		StartedAt: time.Now().UTC(),
	}

	maxPar := plan.MaxParallel
	if maxPar <= 0 || maxPar > len(plan.Leaves) {
		maxPar = len(plan.Leaves)
	}

	sem := make(chan struct{}, maxPar)
	results := make([]LeafResult, len(plan.Leaves))
	var wg sync.WaitGroup

	for i, in := range plan.Leaves {
		i := i
		in := in
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			start := time.Now()
			var entries []ledger.Entry
			var err error
			for attempt := 0; attempt <= plan.RetryEach; attempt++ {
				entries, err = o.Run(ctx, in)
				if err == nil {
					break
				}
			}
			results[i] = LeafResult{
				LeafID:   in.LeafID,
				Node:     in.Node,
				Entries:  entries,
				Err:      err,
				Duration: time.Since(start),
			}
		}()
	}
	wg.Wait()

	// Stable order by LeafID for deterministic reporting.
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].LeafID < results[j].LeafID
	})

	result.Leaves = results
	result.EndedAt = time.Now().UTC()
	result.OK = true
	for _, lr := range results {
		if lr.Err != nil {
			result.OK = false
		}
	}

	// Merge per-leaf entries into the parent ledger.
	if o.Ledger != nil {
		// Parent "swarm_start" marker.
		_ = o.Ledger.Append(plan.TaskID, ledger.Entry{
			TenantID: plan.TenantID,
			Phase:    "swarm",
			Action:   "start:" + plan.ParentRun,
			Result:   ledger.ResultOK,
		})
		for _, lr := range results {
			for _, e := range lr.Entries {
				// Re-emit each leaf entry under the parent task ID, with the
				// leaf's existing phase preserved. The plan's TenantID is
				// authoritative: a leaf runner that left TenantID empty
				// inherits it here so billing entries always carry the
				// customer-plane identity end-to-end. A leaf that already set
				// a different TenantID is left alone — the orchestrator
				// trusts the runner's explicit choice.
				e.TaskID = plan.TaskID
				if e.TenantID == "" {
					e.TenantID = plan.TenantID
				}
				_ = o.Ledger.Append(plan.TaskID, e)
			}
			if lr.Err != nil {
				_ = o.Ledger.Append(plan.TaskID, ledger.Entry{
					TenantID:   plan.TenantID,
					Phase:      "swarm",
					Action:     "leaf-error:" + lr.LeafID,
					Result:     ledger.ResultError,
					DurationMs: lr.Duration.Milliseconds(),
				})
			} else {
				_ = o.Ledger.Append(plan.TaskID, ledger.Entry{
					TenantID:   plan.TenantID,
					Phase:      "swarm",
					Action:     "leaf-ok:" + lr.LeafID,
					Result:     ledger.ResultOK,
					DurationMs: lr.Duration.Milliseconds(),
				})
			}
		}
		final := ledger.ResultOK
		if !result.OK {
			final = ledger.ResultError
		}
		_ = o.Ledger.Append(plan.TaskID, ledger.Entry{
			TenantID: plan.TenantID,
			Phase:    "swarm",
			Action:   "done:" + plan.ParentRun,
			Result:   final,
		})
	}

	return result, nil
}

// Summary returns a one-line human-readable description of the swarm outcome.
func (r *SwarmResult) Summary() string {
	okCount := 0
	for _, l := range r.Leaves {
		if l.Err == nil {
			okCount++
		}
	}
	return fmt.Sprintf("task=%s leaves=%d ok=%d failed=%d duration=%s",
		r.TaskID, len(r.Leaves), okCount, len(r.Leaves)-okCount, r.EndedAt.Sub(r.StartedAt).Round(time.Millisecond))
}
