// submitjob.go implements the fleet.SubmitJob RPC method.
//
// Contract (from owera-cloud's internal/dispatcher):
//
//	method: fleet.SubmitJob
//	params: {tenant_id, sku, cloud_job_id, inputs}
//	result: {task_id}
//
// task_id is the operator-plane ledger task ID — the same value
// `fleetctl ledger show <task-id>` consumes. Every ledger entry written
// for the task carries tenant_id (stamped onto orchestrator.Plan.TenantID
// and onto direct ledger.Entry writes from this handler).
//
// cloud_job_id is the idempotency anchor. A persisted map (cloud_job_id →
// task_id) at <ledgerDir>/.cloud-jobs.json lets us return the existing
// task_id without re-dispatching if the customer plane retries.
//
// SKU dispatch is intentionally minimal in v1. The cloud side has not
// published the final SKU enum yet; this implementation handles:
//
//   - delegate.shell.v1 — single-node shell delegate (inputs: {node, cmd})
//
// Unknown SKUs return CodeInvalidParams with a clear message rather than
// silently no-op'ing — the customer plane needs to know its request was
// rejected so its dispatcher can surface a real error to the caller.
package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/owera/owera-fleet/internal/ledger"
	"github.com/owera/owera-fleet/internal/runregistry"
)

// SubmitJobParams is the wire shape for fleet.SubmitJob.
type SubmitJobParams struct {
	TenantID   string                 `json:"tenant_id"`
	SKU        string                 `json:"sku"`
	CloudJobID string                 `json:"cloud_job_id"`
	Inputs     map[string]interface{} `json:"inputs"`
}

// SubmitJobResult is the wire shape returned to the customer plane.
type SubmitJobResult struct {
	TaskID string `json:"task_id"`
}

// SKURouter dispatches one SubmitJob into the appropriate primitive.
// Implementations are responsible for emitting their own ledger entries
// (every entry MUST carry tenant_id — the handler stamps it onto the
// Plan, but direct ledger.Append calls from a router need to set the
// field explicitly).
//
// Returning a non-nil error from Dispatch causes the RPC to fail with
// CodeInternalError (or whatever *Error the router returns).
//
// # Finite vs long-running routers
//
// Finite routers (e.g. delegate.shell.v1, campaign-swarm@v1) emit a
// terminal `phase: "complete"|"failed"` entry from inside Dispatch and
// return when the job is done. The cloud-side LedgerTailClient watches
// for that terminal entry to advance the customer job to `succeeded` /
// `failed`.
//
// Long-running / subscription routers (e.g. triage-watch@v1) install a
// cronjob (or other persistent worker) inside Dispatch and return nil
// as soon as the worker is wired up; the task stays in the `running`
// state until `fleet.CancelTask` is invoked. Such routers MUST NOT
// emit a terminal ledger entry from Dispatch — doing so would close
// out the customer job prematurely. They opt into this lifecycle by
// implementing the optional [LongRunningRouter] interface (or by
// embedding [LongRunning] for the bool-method-only opt-in).
type SKURouter interface {
	// Dispatch routes inputs into a primitive. taskID is pre-allocated by
	// the handler so the ledger trail can start before any worker is
	// dialed. Implementations should append their own entries to led
	// under taskID.
	Dispatch(ctx context.Context, taskID, tenantID string, inputs map[string]interface{}) error
}

// LongRunningRouter is an optional interface SKURouter implementations
// can satisfy to declare a subscription / cronjob-style lifecycle. When
// SubmitJobHandler sees a router report LongRunning() == true, it skips
// any auto-synthesised terminal ledger entry and leaves the task in the
// `running` state. Cancellation via fleet.CancelTask (which invokes the
// runregistry cancel func) is what eventually writes the terminal
// `cancelled` entry.
//
// Routers that do not implement this interface are treated as finite —
// they are responsible for emitting their own terminal entry before
// Dispatch returns.
//
// The interface is intentionally tiny (single bool method) to keep the
// contract backward-compatible: every router predating H1 still
// satisfies SKURouter and is treated as finite, no recompile required.
type LongRunningRouter interface {
	// LongRunning reports whether this router installs a persistent
	// worker (cronjob, daemon, etc.) and intends the task to remain in
	// the `running` state after Dispatch returns. The handler reads
	// this once per dispatch, immediately after Dispatch returns nil.
	LongRunning() bool
}

// isLongRunning returns true iff r reports itself as long-running. The
// helper centralises the type-assert so handler logic and tests share
// the same probe semantics (a router that does NOT implement
// LongRunningRouter is finite by default).
func isLongRunning(r SKURouter) bool {
	lr, ok := r.(LongRunningRouter)
	return ok && lr.LongRunning()
}

// SKURouterFunc adapts a plain function to the SKURouter interface.
// Function-shaped routers are finite by default; wrap with
// [LongRunningSKURouterFunc] to declare a long-running lifecycle.
type SKURouterFunc func(ctx context.Context, taskID, tenantID string, inputs map[string]interface{}) error

// Dispatch implements SKURouter.
func (f SKURouterFunc) Dispatch(ctx context.Context, taskID, tenantID string, inputs map[string]interface{}) error {
	return f(ctx, taskID, tenantID, inputs)
}

// LongRunningSKURouterFunc adapts a plain function to the SKURouter +
// LongRunningRouter interfaces. Useful in tests and lightweight
// production wiring where a router is just a closure but needs to opt
// into the long-running lifecycle.
type LongRunningSKURouterFunc func(ctx context.Context, taskID, tenantID string, inputs map[string]interface{}) error

// Dispatch implements SKURouter.
func (f LongRunningSKURouterFunc) Dispatch(ctx context.Context, taskID, tenantID string, inputs map[string]interface{}) error {
	return f(ctx, taskID, tenantID, inputs)
}

// LongRunning implements LongRunningRouter, always returning true.
func (f LongRunningSKURouterFunc) LongRunning() bool { return true }

// SubmitJobHandler owns the dependencies the SubmitJob RPC method needs.
// Construct via [NewSubmitJobHandler] and register with
// Server.Register("fleet.SubmitJob", h.Handle).
type SubmitJobHandler struct {
	Ledger    *ledger.Ledger
	LedgerDir string

	// Routers is the SKU → router dispatch table. Populated via
	// [SubmitJobHandler.RegisterSKU]. A missing SKU returns
	// CodeInvalidParams.
	routers map[string]SKURouter
	mu      sync.RWMutex

	// idempotencyPath is the on-disk JSON map (cloud_job_id → task_id).
	// File is mode 0o600. Held under mu for write serialization.
	idempotencyPath string

	// NewTaskID is overridable in tests. Production uses a base32
	// (lowercase) random 8-byte ID with the "task-" prefix to stay
	// compatible with the existing genTaskID() shape in swarm.go.
	NewTaskID func() string

	// Now is overridable in tests for deterministic ledger timestamps.
	Now func() time.Time
}

// NewSubmitJobHandler constructs a handler. ledgerDir is where the
// idempotency map is persisted (alongside the ledger files).
func NewSubmitJobHandler(led *ledger.Ledger, ledgerDir string) *SubmitJobHandler {
	return &SubmitJobHandler{
		Ledger:          led,
		LedgerDir:       ledgerDir,
		routers:         make(map[string]SKURouter),
		idempotencyPath: filepath.Join(ledgerDir, ".cloud-jobs.json"),
		NewTaskID:       defaultNewTaskID,
		Now:             func() time.Time { return time.Now().UTC() },
	}
}

// RegisterSKU adds a router for a SKU name. Duplicate registration panics
// (same convention as Server.Register).
func (h *SubmitJobHandler) RegisterSKU(sku string, r SKURouter) {
	if sku == "" {
		panic("rpc: RegisterSKU called with empty sku")
	}
	if r == nil {
		panic("rpc: RegisterSKU called with nil router for " + sku)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, dup := h.routers[sku]; dup {
		panic("rpc: duplicate SKU registration for " + sku)
	}
	h.routers[sku] = r
}

// Handle is the JSON-RPC handler. Register with
// server.Register("fleet.SubmitJob", h.Handle).
func (h *SubmitJobHandler) Handle(ctx context.Context, raw json.RawMessage) (any, error) {
	var p SubmitJobParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, NewError(CodeInvalidParams, "parse params: "+err.Error())
	}
	if p.TenantID == "" {
		return nil, NewError(CodeInvalidParams, "tenant_id is required")
	}
	if p.SKU == "" {
		return nil, NewError(CodeInvalidParams, "sku is required")
	}
	if p.CloudJobID == "" {
		return nil, NewError(CodeInvalidParams, "cloud_job_id is required")
	}

	// Idempotency check: if we've already seen this cloud_job_id, return
	// the stored task_id without re-dispatching. The customer plane's
	// at-least-once retry path lands here.
	if existing, ok := h.lookupCloudJob(p.CloudJobID); ok {
		return SubmitJobResult{TaskID: existing}, nil
	}

	h.mu.RLock()
	router, ok := h.routers[p.SKU]
	h.mu.RUnlock()
	if !ok {
		return nil, NewError(CodeInvalidParams, "unknown sku: "+p.SKU)
	}

	taskID := h.NewTaskID()

	// Emit a "submit" marker into the ledger before dispatch. Carries
	// tenant_id so even a router that crashes before its first write
	// leaves an attributable trail.
	if h.Ledger != nil {
		dataJSON, _ := json.Marshal(map[string]string{
			"sku":          p.SKU,
			"cloud_job_id": p.CloudJobID,
		})
		_ = h.Ledger.Append(taskID, ledger.Entry{
			Ts:       h.Now(),
			TaskID:   taskID,
			TenantID: p.TenantID,
			Phase:    "submit",
			Action:   "fleet.SubmitJob",
			Result:   ledger.ResultOK,
			Data:     dataJSON,
		})
	}

	// Record the mapping BEFORE dispatch so a router crash + retry still
	// returns the same task_id (the second call sees the existing
	// mapping and short-circuits). Worst case: the router fails and the
	// task_id maps to a ledger with only the submit marker. That is
	// still better than minting a fresh task_id on every retry.
	if err := h.recordCloudJob(p.CloudJobID, taskID); err != nil {
		// Persistence failure does not block dispatch — the operator
		// can rebuild the map from the ledger trail. Log it via an
		// error entry so we don't silently lose the signal.
		if h.Ledger != nil {
			_ = h.Ledger.Append(taskID, ledger.Entry{
				Ts:         h.Now(),
				TaskID:     taskID,
				TenantID:   p.TenantID,
				Phase:      "submit",
				Action:     "idempotency-persist-failed",
				Result:     ledger.ResultError,
				StderrTail: err.Error(),
			})
		}
	}

	// Register a cancellable ctx under taskID so fleet.CancelTask can
	// interrupt the work via runregistry.
	//
	// Two lifecycles:
	//
	//   - Finite routers: dispatch ctx is derived from the request ctx
	//     and torn down when Handle returns. runregistry registration is
	//     scoped to the duration of Dispatch.
	//
	//   - Long-running routers: dispatch ctx is derived from
	//     context.Background() because the persistent worker (cronjob,
	//     daemon, etc.) must outlive the inbound JSON-RPC request. The
	//     runregistry entry stays in place after Handle returns so
	//     fleet.CancelTask can find it and tear the worker down. The
	//     router itself is responsible for emitting the terminal
	//     `cancelled` entry when its ctx is cancelled.
	longRunning := isLongRunning(router)
	var (
		dispatchCtx context.Context
		cancel      context.CancelFunc
	)
	if longRunning {
		dispatchCtx, cancel = context.WithCancel(context.Background())
	} else {
		dispatchCtx, cancel = context.WithCancel(ctx)
	}
	runregistry.Register(taskID, cancel)
	// Cleanup runs only for finite routers. For long-running, the
	// runregistry entry + ctx survive Handle's return; fleet.CancelTask
	// (via runregistry.Cancel) is the one path that invokes cancel and
	// removes the registration.
	if !longRunning {
		defer cancel()
		defer runregistry.Unregister(taskID)
	}

	if err := router.Dispatch(dispatchCtx, taskID, p.TenantID, p.Inputs); err != nil {
		// On dispatch error, always tear down — even for long-running
		// routers — because the persistent worker never got wired up.
		if longRunning {
			cancel()
			runregistry.Unregister(taskID)
		}
		if h.Ledger != nil {
			_ = h.Ledger.Append(taskID, ledger.Entry{
				Ts:         h.Now(),
				TaskID:     taskID,
				TenantID:   p.TenantID,
				Phase:      "submit",
				Action:     "dispatch-failed",
				Result:     ledger.ResultError,
				StderrTail: err.Error(),
			})
		}
		var rpcErr *Error
		if errors.As(err, &rpcErr) {
			return nil, rpcErr
		}
		return nil, NewError(CodeInternalError, "dispatch: "+err.Error())
	}

	// For long-running routers we deliberately do NOT synthesise any
	// terminal ledger entry here — the task stays in `running` state
	// until cancellation. Finite routers are expected to have written
	// their own `complete` / `failed` entry from inside Dispatch before
	// returning nil.
	return SubmitJobResult{TaskID: taskID}, nil
}

// lookupCloudJob returns the existing task_id for cloudJobID, or false if
// none. Errors reading the map are swallowed — a missing/corrupt file
// just means "no idempotency record", which is safe (we'll mint a new
// task_id, dispatch fresh, and overwrite the map on next write).
func (h *SubmitJobHandler) lookupCloudJob(cloudJobID string) (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	m, err := readCloudJobMap(h.idempotencyPath)
	if err != nil {
		return "", false
	}
	id, ok := m[cloudJobID]
	return id, ok
}

// recordCloudJob persists the cloud_job_id → task_id mapping. Atomic via
// write-temp-then-rename so a crash mid-write does not corrupt the file.
func (h *SubmitJobHandler) recordCloudJob(cloudJobID, taskID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, err := readCloudJobMap(h.idempotencyPath)
	if err != nil {
		m = make(map[string]string)
	}
	m[cloudJobID] = taskID
	return writeCloudJobMap(h.idempotencyPath, m)
}

// readCloudJobMap loads the cloud_job_id → task_id table. Returns an
// empty (non-nil) map if the file does not exist.
func readCloudJobMap(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]string), nil
	}
	if err != nil {
		return nil, err
	}
	m := make(map[string]string)
	if len(raw) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return m, nil
}

// writeCloudJobMap persists m atomically.
func writeCloudJobMap(path string, m map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// defaultNewTaskID returns "task-" + lowercase hex of 8 random bytes.
// Matches the shape of swarm.go's genTaskID() so audit tooling stays
// consistent regardless of submission path.
func defaultNewTaskID() string {
	b := make([]byte, 8)
	if _, err := readRandom(b); err != nil {
		// crypto/rand failure on macOS / Linux is effectively
		// unrecoverable; fall back to the timestamp so we still emit
		// SOMETHING unique.
		return fmt.Sprintf("task-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("task-%x", b)
}

// readRandom is overridable in tests. Production wraps crypto/rand.Read.
var readRandom = func(b []byte) (int, error) {
	return cryptoRead(b)
}

// ShellDelegateRouter is the built-in router for SKU "delegate.shell.v1".
// It expects inputs of shape {"node": "user@host", "cmd": "..."} and
// dispatches via a caller-supplied DelegateFunc so the rpc package does
// not need to import internal/ssh. The cmd/fleetctl wiring layer injects
// the real delegate primitive.
type ShellDelegateRouter struct {
	Ledger *ledger.Ledger

	// Delegate runs cmd on node and returns the per-leaf ledger entries
	// to merge under taskID. The router stamps tenant_id onto each
	// returned entry before appending — implementations need not set it.
	Delegate DelegateFunc

	// Now is overridable in tests.
	Now func() time.Time
}

// DelegateFunc is the injected primitive for ShellDelegateRouter. It is
// expected to perform the SSH dial + command run and return one or more
// ledger entries describing the outcome. Errors signal a transport-level
// failure (dial, ssh handshake, context cancellation); a non-zero remote
// exit code should be encoded as a ledger.Entry with Result=Error rather
// than a Go error return.
type DelegateFunc func(ctx context.Context, node, cmd string) ([]ledger.Entry, error)

// Dispatch implements SKURouter for ShellDelegateRouter.
func (r *ShellDelegateRouter) Dispatch(ctx context.Context, taskID, tenantID string, inputs map[string]interface{}) error {
	node, _ := inputs["node"].(string)
	cmd, _ := inputs["cmd"].(string)
	if node == "" {
		return NewError(CodeInvalidParams, "delegate.shell.v1: inputs.node is required")
	}
	if cmd == "" {
		return NewError(CodeInvalidParams, "delegate.shell.v1: inputs.cmd is required")
	}
	if r.Delegate == nil {
		return NewError(CodeInternalError, "delegate.shell.v1: no Delegate func registered")
	}

	now := r.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	if r.Ledger != nil {
		_ = r.Ledger.Append(taskID, ledger.Entry{
			Ts:       now(),
			TaskID:   taskID,
			TenantID: tenantID,
			Phase:    "delegate",
			Action:   "dispatch:" + node,
			Result:   ledger.ResultOK,
		})
	}

	entries, err := r.Delegate(ctx, node, cmd)
	if r.Ledger != nil {
		for _, e := range entries {
			e.TaskID = taskID
			e.TenantID = tenantID
			if e.Ts.IsZero() {
				e.Ts = now()
			}
			_ = r.Ledger.Append(taskID, e)
		}
	}
	if err != nil {
		if r.Ledger != nil {
			_ = r.Ledger.Append(taskID, ledger.Entry{
				Ts:         now(),
				TaskID:     taskID,
				TenantID:   tenantID,
				Phase:      "delegate",
				Action:     "transport-error:" + node,
				Result:     ledger.ResultError,
				StderrTail: err.Error(),
			})
		}
		return err
	}
	return nil
}

// NOTE: a helper to build an orchestrator.Plan with TenantID was removed
// pending the parallel #5 work that adds orchestrator.Plan.TenantID. Once
// that field lands, future SKU routers that fan out via the orchestrator
// path should construct the Plan directly and set Plan.TenantID =
// tenantID. The ledger entries the orchestrator merges will then inherit
// the tenant stamp through Entry.TenantID copy on append.
