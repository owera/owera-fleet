// Package runregistry maps in-flight task IDs to their cancel funcs so that an
// out-of-band cancellation request (e.g. fleet.CancelTask RPC) can interrupt
// work already running on the gateway/orchestrator.
//
// Workers (delegate primitives, swarm orchestrator, cron-driven jobs, etc.)
// call [Register] when they begin work for a task and [Unregister] when they
// finish (success or error). A controller calling [Cancel] looks up the task's
// cancel func, invokes it, and removes the registration in one step.
//
// The registry is in-process only — there is no persistence. A gateway restart
// loses all registrations, which is fine because in-flight work tied to the
// previous process is also gone.
//
// All operations are safe for concurrent use.
package runregistry

import (
	"context"
	"sync"
)

// Registry holds the live task → cancel-func mapping.
//
// The zero value is ready to use.
type Registry struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

// Default is the process-wide registry that gateway services share. It is the
// instance the CancelTask RPC handler consults and the one the delegate/swarm
// primitives register themselves against unless they explicitly inject a
// scoped one (e.g. for tests).
var Default = &Registry{}

// Register associates taskID with cancel. If taskID was already registered,
// the previous cancel func is silently replaced — the caller is responsible
// for not double-registering the same task ID in production code. Tests rely
// on this overwrite behavior so they can swap funcs without unregistering.
func (r *Registry) Register(taskID string, cancel context.CancelFunc) {
	if taskID == "" || cancel == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancels == nil {
		r.cancels = make(map[string]context.CancelFunc)
	}
	r.cancels[taskID] = cancel
}

// Unregister removes taskID from the registry without invoking its cancel
// func. Use when work finishes normally (success or error).
func (r *Registry) Unregister(taskID string) {
	if taskID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cancels, taskID)
}

// Cancel looks up taskID, calls its cancel func, and removes the
// registration. Returns true if a registration was found and invoked, false
// if the task was not registered (already done or never started).
func (r *Registry) Cancel(taskID string) bool {
	if taskID == "" {
		return false
	}
	r.mu.Lock()
	cancel, ok := r.cancels[taskID]
	if ok {
		delete(r.cancels, taskID)
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// Has reports whether taskID is currently registered. Intended for tests and
// diagnostics; production code should call [Cancel] which atomically
// inspects-and-removes.
func (r *Registry) Has(taskID string) bool {
	if taskID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.cancels[taskID]
	return ok
}

// Len returns the number of currently-registered tasks. For diagnostics only.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cancels)
}

// Package-level convenience wrappers around [Default] so callers can write
// `runregistry.Register(...)` instead of threading a Registry value through.

// Register is shorthand for Default.Register.
func Register(taskID string, cancel context.CancelFunc) {
	Default.Register(taskID, cancel)
}

// Unregister is shorthand for Default.Unregister.
func Unregister(taskID string) {
	Default.Unregister(taskID)
}

// Cancel is shorthand for Default.Cancel.
func Cancel(taskID string) bool {
	return Default.Cancel(taskID)
}

// Has is shorthand for Default.Has.
func Has(taskID string) bool {
	return Default.Has(taskID)
}
