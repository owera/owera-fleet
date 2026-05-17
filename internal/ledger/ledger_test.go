package ledger_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

// TestAppendConcurrent — N goroutines all append to the same task file
// in parallel. After they all finish, Read must return exactly N entries
// and every signature must verify. Pre-fix (no fsync) this could still
// race on the os.OpenFile + Write + Close cycle if Append's internals
// regress; pins the contract for the future.
func TestAppendConcurrent(t *testing.T) {
	const N = 64
	dir := t.TempDir()
	l, err := ledger.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var wg sync.WaitGroup
	var failed int32
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := l.Append("concurrent", ledger.Entry{
				Phase:  "p",
				Action: fmt.Sprintf("a-%d", i),
				Result: ledger.ResultOK,
			}); err != nil {
				t.Errorf("Append %d: %v", i, err)
				failed = 1
			}
		}(i)
	}
	wg.Wait()
	if failed != 0 {
		return
	}
	entries, err := l.Read("concurrent")
	if err != nil {
		t.Fatalf("Read after concurrent append: %v", err)
	}
	if len(entries) != N {
		t.Errorf("got %d entries, want %d — concurrent appends raced or were lost", len(entries), N)
	}
}

// TestTruncatedTailSurfacesError — write three entries, then truncate
// the file in the middle of the last line. Read must surface a clear
// parse or signature error rather than silently returning two clean
// entries plus a half-line that looked like a different valid entry.
func TestTruncatedTailSurfacesError(t *testing.T) {
	dir := t.TempDir()
	l, err := ledger.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := l.Append("trunc", ledger.Entry{
			Phase:  "p",
			Action: fmt.Sprintf("a-%d", i),
			Result: ledger.ResultOK,
		}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	path := filepath.Join(dir, "trunc.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Chop off the last 40 bytes — enough to mangle the final line's
	// signature without removing the entire third entry. The truncated
	// line is no longer valid JSON or has a bad signature; either is an
	// error.
	if len(data) < 60 {
		t.Fatalf("file too small to truncate meaningfully: %d bytes", len(data))
	}
	if err := os.WriteFile(path, data[:len(data)-40], 0o600); err != nil {
		t.Fatalf("rewrite truncated: %v", err)
	}
	_, err = l.Read("trunc")
	if err == nil {
		t.Errorf("Read should surface an error for a truncated tail line; got nil")
	}
}
