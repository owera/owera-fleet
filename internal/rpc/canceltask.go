// Package rpc holds the JSON-RPC handlers fleetctl serve exposes over the
// Cloudflare tunnel. The shared Server type + Handler signature are owned by
// agent #2 (issue #2). This file (canceltask.go, issue #3) provides the
// CancelTask handler in a form ready to be registered:
//
//	// In server.go (agent #2):
//	//   srv.Register("fleet.CancelTask", rpc.CancelTask)
//
// We export the handler as both:
//   - [CancelTask] — the package-scoped function with the framework's
//     expected signature, suitable for direct `srv.Register` use.
//   - [Handle] — a thin standalone wrapper that lets tests exercise the
//     handler without going through Server, which is useful while #2 is
//     still in flight on a parallel branch.
//
// The handler is intentionally stateless: it consults
// [runregistry.Default] to find the task's cancel func, invokes it, and
// reports back. All ledger writing for the cancelled run happens inside the
// orchestrator (which observes ctx.Done()), not here.
package rpc

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/owera/owera-fleet/internal/runregistry"
)

// CancelTaskParams matches the JSON-RPC `params` object for fleet.CancelTask.
//
//	{ "task_id": "..." }
type CancelTaskParams struct {
	TaskID string `json:"task_id"`
}

// CancelTaskResult is the response body. `cancelled` is true if a cancel
// signal was delivered to a live in-flight task; false if the task was
// already done (no-op) or was never registered.
type CancelTaskResult struct {
	Cancelled bool `json:"cancelled"`
}

// ErrMissingTaskID is returned (and surfaced as a JSON-RPC error by the
// server framework) when the request omits task_id.
var ErrMissingTaskID = errors.New("rpc: cancel_task: task_id is required")

// CancelTask is the handler function with the signature agent #2's framework
// will register against ("fleet.CancelTask"). Wire-up lives in cmd/fleetctl
// serve.go (agent #2 owns that file).
//
// Contract:
//   - params is the JSON-encoded [CancelTaskParams].
//   - return value is [CancelTaskResult]; the framework marshals it.
//   - cancellation propagates through [runregistry.Default] →
//     context.CancelFunc → in-flight delegate / swarm work.
//   - no ledger write happens here; the orchestrator (or delegate primitive)
//     observes its ctx being cancelled and writes the explicit
//     `{phase:"swarm"|"task", action:"cancel", result:"cancelled"}` entry.
func CancelTask(_ context.Context, params json.RawMessage) (any, error) {
	var p CancelTaskParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
	}
	if p.TaskID == "" {
		return nil, ErrMissingTaskID
	}
	ok := runregistry.Cancel(p.TaskID)
	return CancelTaskResult{Cancelled: ok}, nil
}

// Handle is a convenience entry point for tests and embedders that want the
// concrete result type back without round-tripping through `any`. The
// production wire path goes through [CancelTask].
func Handle(ctx context.Context, params json.RawMessage) (CancelTaskResult, error) {
	out, err := CancelTask(ctx, params)
	if err != nil {
		return CancelTaskResult{}, err
	}
	res, ok := out.(CancelTaskResult)
	if !ok {
		return CancelTaskResult{}, errors.New("rpc: cancel_task: internal: unexpected result type")
	}
	return res, nil
}
