// Package rpc — `fleet.LedgerTail` returns the entries of a task ledger
// strictly after a caller-supplied cursor. It exists so the customer-
// plane dispatcher (`api/internal/dispatcher.Worker`) can poll per-job
// progress without re-reading the whole file on every cycle.
//
// # Cursor semantics
//
//   - First call: client passes `after_ts` as RFC3339 of zero (or
//     omitted). Server returns every entry.
//   - Subsequent calls: client passes the `cursor` from the previous
//     response. Server returns only entries strictly after that Ts.
//   - End-of-stream is signalled by an empty `entries` array. The cursor
//     stays the same; the client should keep polling until it sees a
//     terminal entry (Phase=="complete" || Phase=="failed").
//
// # Why not server-side streaming
//
// SSE / WebSocket would reduce poll overhead but force a long-lived
// connection through the Cloudflare tunnel. The tunnel is sized for
// request/response; sustained connections at fleet scale haven't been
// measured. Polling at 1s cadence is well under budget for V0/V1
// volumes, so the streaming refactor is deferred until concurrency
// becomes a metric we measure.
package rpc

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/owera/owera-fleet/internal/ledger"
)

// LedgerTailParams is the JSON-RPC params shape for `fleet.LedgerTail`.
//
// Tasks live for the duration of a customer job (minutes to hours, max
// 24 h per SLA). After complete/failed, the ledger file is still
// readable for the retention window (per `compliance/policies/data-
// retention-policy.md`); polling after terminal entry is permitted but
// returns nothing new.
type LedgerTailParams struct {
	TaskID  string `json:"task_id"`
	AfterTs string `json:"after_ts,omitempty"` // RFC3339; empty = read all
}

// LedgerTailResult is the JSON-RPC result shape. Cursor is the latest
// Ts among the returned entries (RFC3339), or the request's AfterTs
// when no new entries were available — so a client can always echo it
// back on the next call.
type LedgerTailResult struct {
	Entries []ledger.Entry `json:"entries"`
	Cursor  string         `json:"cursor"`
}

// LedgerTailHandler binds a ledger reader (intentionally just the
// reader surface, not the full *Ledger) to the JSON-RPC handler.
type LedgerTailHandler struct {
	led LedgerReader
}

// LedgerReader is the subset of *ledger.Ledger we need. Stated as an
// interface so tests can pass a fake without spinning up a real signed
// ledger.
type LedgerReader interface {
	ReadSince(taskID string, after time.Time) ([]ledger.Entry, error)
}

// NewLedgerTailHandler wires the handler. led may be a *ledger.Ledger
// opened read-only or read-write — only ReadSince is called.
func NewLedgerTailHandler(led LedgerReader) *LedgerTailHandler {
	return &LedgerTailHandler{led: led}
}

// Handle implements the JSON-RPC Handler signature.
func (h *LedgerTailHandler) Handle(_ context.Context, raw json.RawMessage) (any, error) {
	var p LedgerTailParams
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, NewError(CodeInvalidParams, "ledgertail: bad params: "+err.Error())
		}
	}
	if p.TaskID == "" {
		return nil, NewError(CodeInvalidParams, "ledgertail: task_id required")
	}

	var after time.Time
	if p.AfterTs != "" {
		t, err := time.Parse(time.RFC3339Nano, p.AfterTs)
		if err != nil {
			t, err = time.Parse(time.RFC3339, p.AfterTs)
			if err != nil {
				return nil, NewError(CodeInvalidParams, "ledgertail: bad after_ts: "+err.Error())
			}
		}
		after = t
	}

	entries, err := h.led.ReadSince(p.TaskID, after)
	if err != nil {
		// "Task has no ledger yet" is the common first-poll race after
		// SubmitJob — return empty rather than surfacing it as a 500.
		// The ledger wraps the OS error with fmt.Errorf, so we
		// substring-match on the only stable shape it produces.
		msg := err.Error()
		if strings.Contains(msg, "no such file or directory") ||
			strings.Contains(msg, "file does not exist") {
			return LedgerTailResult{
				Entries: []ledger.Entry{},
				Cursor:  p.AfterTs,
			}, nil
		}
		return nil, NewError(CodeInternalError, "ledgertail: read: "+err.Error())
	}

	cursor := p.AfterTs
	if n := len(entries); n > 0 {
		cursor = entries[n-1].Ts.UTC().Format(time.RFC3339Nano)
	}
	return LedgerTailResult{Entries: entries, Cursor: cursor}, nil
}
