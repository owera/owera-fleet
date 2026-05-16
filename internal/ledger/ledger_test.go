package ledger_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/owera/owera-fleet/internal/ledger"
)

func TestAppendAndRead(t *testing.T) {
	dir := t.TempDir()
	l, err := ledger.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	e := ledger.Entry{
		TaskID:     "task-1",
		Phase:      "test",
		Action:     "append",
		Result:     ledger.ResultOK,
		DurationMs: 42,
	}
	if err := l.Append("task-1", e); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := l.Append("task-1", ledger.Entry{Phase: "test", Action: "second", Result: ledger.ResultBill}); err != nil {
		t.Fatalf("Append second: %v", err)
	}

	entries, err := l.Read("task-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Action != "append" {
		t.Errorf("entry[0].Action = %q", entries[0].Action)
	}
	if entries[0].Ts.IsZero() {
		t.Error("Ts should be filled")
	}
	if entries[0].Sig == "" {
		t.Error("Sig should be set")
	}
}

func TestTamperedEntryDetected(t *testing.T) {
	dir := t.TempDir()
	l, err := ledger.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Append("tamper", ledger.Entry{Phase: "p", Action: "a", Result: ledger.ResultOK}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Corrupt the ledger file.
	path := filepath.Join(dir, "tamper.jsonl")
	data, _ := os.ReadFile(path)
	var e ledger.Entry
	_ = json.Unmarshal(data, &e)
	e.Result = ledger.ResultBill // tamper
	corrupted, _ := json.Marshal(e)
	_ = os.WriteFile(path, append(corrupted, '\n'), 0o600)

	_, err = l.Read("tamper")
	if !errors.Is(err, ledger.ErrBadSignature) {
		t.Errorf("expected ErrBadSignature, got %v", err)
	}
}

func TestTasksList(t *testing.T) {
	dir := t.TempDir()
	l, err := ledger.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, id := range []string{"z-task", "a-task", "m-task"} {
		if err := l.Append(id, ledger.Entry{Phase: "p", Action: "a", Result: ledger.ResultOK}); err != nil {
			t.Fatalf("Append %s: %v", id, err)
		}
	}
	ids, err := l.Tasks()
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(ids))
	}
	if ids[0] != "a-task" {
		t.Errorf("tasks not sorted: %v", ids)
	}
}

func TestVerifyAll(t *testing.T) {
	dir := t.TempDir()
	l, err := ledger.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, id := range []string{"t1", "t2"} {
		_ = l.Append(id, ledger.Entry{Phase: "p", Action: "a", Result: ledger.ResultOK})
	}
	results, err := l.VerifyAll()
	if err != nil {
		t.Fatalf("VerifyAll: %v", err)
	}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("task %s: %v", r.TaskID, r.Err)
		}
	}
}

func TestOpenReadOnly(t *testing.T) {
	dir := t.TempDir()
	l, err := ledger.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = l.Append("ro", ledger.Entry{Phase: "p", Action: "a", Result: ledger.ResultOK})

	ro, err := ledger.OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	entries, err := ro.Read("ro")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(entries))
	}
	// Append must fail on read-only ledger.
	if err := ro.Append("ro", ledger.Entry{Phase: "p", Action: "b", Result: ledger.ResultOK}); err == nil {
		t.Error("expected error from read-only Append")
	}
}

func TestTimestampFilled(t *testing.T) {
	dir := t.TempDir()
	l, _ := ledger.Open(dir)
	before := time.Now().UTC()
	_ = l.Append("ts", ledger.Entry{Phase: "p", Action: "a", Result: ledger.ResultOK})
	after := time.Now().UTC()
	entries, _ := l.Read("ts")
	if entries[0].Ts.Before(before) || entries[0].Ts.After(after) {
		t.Errorf("Ts %v not in [%v, %v]", entries[0].Ts, before, after)
	}
}
