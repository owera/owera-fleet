package rpc

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/owera/owera-fleet/internal/ledger"
)

// TestLedgerTail_CursorAdvance is the happy path: append three entries
// to a task, tail with empty cursor (read all), then tail again with
// the returned cursor (read nothing new), then append a fourth and
// tail again (read only the fourth).
func TestLedgerTail_CursorAdvance(t *testing.T) {
	dir := t.TempDir()
	led, err := ledger.Open(filepath.Join(dir, "led"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	const taskID = "task-tail-1"
	for i := 0; i < 3; i++ {
		if err := led.Append(taskID, ledger.Entry{
			Phase:  "step",
			Action: "act",
			Result: ledger.ResultOK,
		}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		// Force monotonically-increasing Ts so cursor semantics are deterministic.
		time.Sleep(2 * time.Millisecond)
	}

	h := NewLedgerTailHandler(led)

	// First poll — empty cursor → all 3 entries.
	res1 := callTail(t, h, taskID, "")
	if len(res1.Entries) != 3 {
		t.Fatalf("first poll: got %d entries, want 3", len(res1.Entries))
	}
	if res1.Cursor == "" {
		t.Fatalf("first poll: cursor empty")
	}

	// Second poll — same cursor, no new entries → empty + same cursor.
	res2 := callTail(t, h, taskID, res1.Cursor)
	if len(res2.Entries) != 0 {
		t.Fatalf("second poll: got %d entries, want 0", len(res2.Entries))
	}
	if res2.Cursor != res1.Cursor {
		t.Errorf("second poll: cursor advanced unexpectedly: %q → %q", res1.Cursor, res2.Cursor)
	}

	// Append a fourth entry; third poll returns only that one.
	time.Sleep(2 * time.Millisecond)
	if err := led.Append(taskID, ledger.Entry{
		Phase:  "complete",
		Action: "done",
		Result: ledger.ResultBill,
	}); err != nil {
		t.Fatalf("Append 4th: %v", err)
	}
	res3 := callTail(t, h, taskID, res2.Cursor)
	if len(res3.Entries) != 1 {
		t.Fatalf("third poll: got %d entries, want 1", len(res3.Entries))
	}
	if res3.Entries[0].Phase != "complete" {
		t.Errorf("third poll entry: phase = %q, want 'complete'", res3.Entries[0].Phase)
	}
	if res3.Cursor == res2.Cursor {
		t.Errorf("third poll: cursor did not advance: still %q", res3.Cursor)
	}
}

// TestLedgerTail_MissingTaskReturnsEmpty is the SubmitJob race: cloud
// polls LedgerTail before the operator has written the first entry.
// Must NOT 500; must return empty + the request's cursor unchanged.
func TestLedgerTail_MissingTaskReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	led, err := ledger.Open(filepath.Join(dir, "led"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	h := NewLedgerTailHandler(led)

	res := callTail(t, h, "task-never-existed", "")
	if len(res.Entries) != 0 {
		t.Errorf("missing task: got %d entries, want 0", len(res.Entries))
	}
	if res.Cursor != "" {
		t.Errorf("missing task: cursor = %q, want empty", res.Cursor)
	}
}

// TestLedgerTail_BadParams covers the validation error paths.
func TestLedgerTail_BadParams(t *testing.T) {
	dir := t.TempDir()
	led, _ := ledger.Open(filepath.Join(dir, "led"))
	h := NewLedgerTailHandler(led)

	cases := []struct {
		name   string
		params string
		want   int
	}{
		{"empty task_id", `{"task_id":""}`, CodeInvalidParams},
		{"missing task_id", `{}`, CodeInvalidParams},
		{"bad after_ts", `{"task_id":"x","after_ts":"not-a-time"}`, CodeInvalidParams},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := h.Handle(context.Background(), json.RawMessage(c.params))
			if err == nil {
				t.Fatal("expected error")
			}
			re, ok := err.(*Error)
			if !ok {
				t.Fatalf("expected *Error, got %T: %v", err, err)
			}
			if re.Code != c.want {
				t.Errorf("code: got %d, want %d", re.Code, c.want)
			}
		})
	}
}

// callTail invokes the handler with the given task/cursor and decodes
// the JSON-RPC `result` into LedgerTailResult.
func callTail(t *testing.T, h *LedgerTailHandler, taskID, after string) LedgerTailResult {
	t.Helper()
	params, err := json.Marshal(LedgerTailParams{TaskID: taskID, AfterTs: after})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw, err := h.Handle(context.Background(), params)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// The handler returns the result struct directly (the JSON-RPC
	// server is responsible for envelope encoding). Cast or re-marshal
	// to LedgerTailResult depending on the runtime type.
	if r, ok := raw.(LedgerTailResult); ok {
		return r
	}
	b, _ := json.Marshal(raw)
	var r LedgerTailResult
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return r
}
