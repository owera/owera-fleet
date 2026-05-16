package log

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixedClock(t *testing.T) func() time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, "2026-05-16T15:30:00.123456789Z")
	if err != nil {
		t.Fatalf("setup: parse fixed time: %v", err)
	}
	return func() time.Time { return ts }
}

func decodeLine(t *testing.T, line []byte) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	return got
}

func TestResultValid(t *testing.T) {
	cases := []struct {
		r    Result
		want bool
	}{
		{ResultOK, true},
		{ResultNoChange, true},
		{ResultError, true},
		{ResultSkipped, true},
		{ResultWarn, true},
		{Result(""), false},
		{Result("success"), false},
	}
	for _, c := range cases {
		if got := c.r.Valid(); got != c.want {
			t.Errorf("Result(%q).Valid() = %v, want %v", c.r, got, c.want)
		}
	}
}

func TestAction_OK_Schema(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	l.SetClock(fixedClock(t))

	err := l.Action(Event{
		Node:       "hermes@claw1.local",
		Phase:      "delegate",
		Action:     "run-command",
		Result:     ResultOK,
		DurationMs: 142,
	})
	if err != nil {
		t.Fatalf("Action: %v", err)
	}

	line := buf.Bytes()
	if !bytes.HasSuffix(line, []byte{'\n'}) {
		t.Fatalf("output missing newline: %q", line)
	}
	got := decodeLine(t, bytes.TrimRight(line, "\n"))

	want := map[string]any{
		"ts":          "2026-05-16T15:30:00.123456789Z",
		"node":        "hermes@claw1.local",
		"phase":       "delegate",
		"action":      "run-command",
		"result":      "ok",
		"duration_ms": float64(142),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("field %q = %v, want %v", k, got[k], v)
		}
	}
	if _, present := got["stderr_tail"]; present {
		t.Errorf("stderr_tail should be omitted when empty; got line=%q", line)
	}
}

func TestAction_NoChange_Idempotent(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	l.SetClock(fixedClock(t))

	for i := 0; i < 3; i++ {
		if err := l.Action(Event{
			Node:   "gateway",
			Phase:  "bootstrap",
			Action: "phase-00-brew",
			Result: ResultNoChange,
		}); err != nil {
			t.Fatalf("Action: %v", err)
		}
	}
	lines := bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte{'\n'})
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	for i, ln := range lines {
		got := decodeLine(t, ln)
		if got["result"] != string(ResultNoChange) {
			t.Errorf("line %d: result = %v, want %s", i, got["result"], ResultNoChange)
		}
	}
}

func TestAction_Error_WithStderrTail(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	l.SetClock(fixedClock(t))

	err := l.Action(Event{
		Node:       "hermes@claw2.local",
		Phase:      "delegate",
		Action:     "run-command",
		Result:     ResultError,
		DurationMs: 12,
		StderrTail: "ssh: connect to host claw2.local port 22: Connection refused",
	})
	if err != nil {
		t.Fatalf("Action: %v", err)
	}

	got := decodeLine(t, bytes.TrimRight(buf.Bytes(), "\n"))
	if got["stderr_tail"] == nil {
		t.Fatalf("stderr_tail missing: %s", buf.String())
	}
	if !strings.Contains(got["stderr_tail"].(string), "Connection refused") {
		t.Errorf("stderr_tail = %v, want substring 'Connection refused'", got["stderr_tail"])
	}
}

func TestAction_StderrTailTruncatedFromHead(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)

	// Build a stderr longer than MaxStderrTailBytes; preserve a marker at the tail.
	head := strings.Repeat("A", MaxStderrTailBytes+512)
	marker := "[final-error-marker]"
	stderr := head + marker

	err := l.Action(Event{
		Phase:      "delegate",
		Action:     "long-noisy-fail",
		Result:     ResultError,
		StderrTail: stderr,
	})
	if err != nil {
		t.Fatalf("Action: %v", err)
	}
	got := decodeLine(t, bytes.TrimRight(buf.Bytes(), "\n"))
	tail := got["stderr_tail"].(string)
	if len(tail) > MaxStderrTailBytes {
		t.Errorf("stderr_tail not truncated: len=%d", len(tail))
	}
	if !strings.HasSuffix(tail, marker) {
		t.Errorf("stderr_tail should preserve trailing marker; got suffix %q", tail[len(tail)-len(marker):])
	}
}

func TestAction_InvalidResult(t *testing.T) {
	l := New(&bytes.Buffer{})
	err := l.Action(Event{Result: Result("bogus")})
	if err == nil {
		t.Fatal("expected error for invalid Result")
	}
	if !strings.Contains(err.Error(), "invalid Result") {
		t.Errorf("error = %v, want 'invalid Result'", err)
	}
}

func TestAction_FillsZeroTimestamp(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	l.SetClock(fixedClock(t))

	err := l.Action(Event{
		Phase:  "delegate",
		Action: "ts-default",
		Result: ResultOK,
	})
	if err != nil {
		t.Fatalf("Action: %v", err)
	}
	got := decodeLine(t, bytes.TrimRight(buf.Bytes(), "\n"))
	if got["ts"] != "2026-05-16T15:30:00.123456789Z" {
		t.Errorf("zero ts not filled: got %v", got["ts"])
	}
}

func TestAction_KeepsCallerTimestamp(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	l.SetClock(fixedClock(t))

	caller := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	err := l.Action(Event{
		Timestamp: caller,
		Phase:     "delegate",
		Action:    "ts-explicit",
		Result:    ResultOK,
	})
	if err != nil {
		t.Fatalf("Action: %v", err)
	}
	got := decodeLine(t, bytes.TrimRight(buf.Bytes(), "\n"))
	if got["ts"] != "2025-01-01T00:00:00Z" {
		t.Errorf("caller-provided ts overwritten: got %v", got["ts"])
	}
}

func TestOpen_AppendsAndCreatesDirs(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "nested", "dirs", "delegate.jsonl")

	// First open + write.
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Action(Event{Phase: "p", Action: "a", Result: ResultOK}); err != nil {
		t.Fatalf("Action: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Second open should append, not truncate.
	l2, err := Open(path)
	if err != nil {
		t.Fatalf("Open (append): %v", err)
	}
	if err := l2.Action(Event{Phase: "p", Action: "a", Result: ResultNoChange}); err != nil {
		t.Fatalf("Action 2: %v", err)
	}
	if err := l2.Close(); err != nil {
		t.Fatalf("Close 2: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines after append, got %d (data=%q)", len(lines), data)
	}
}

func TestClosedLoggerRejects(t *testing.T) {
	tmp := t.TempDir()
	l, err := Open(filepath.Join(tmp, "x.jsonl"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err = l.Action(Event{Phase: "p", Action: "a", Result: ResultOK})
	if !errors.Is(err, ErrClosed) {
		t.Errorf("expected ErrClosed, got %v", err)
	}
}

func TestConcurrentActionsDoNotInterleave(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)

	const writers = 8
	const perWriter = 50
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				if err := l.Action(Event{
					Phase:  "concurrency",
					Action: "write",
					Result: ResultOK,
				}); err != nil {
					t.Errorf("Action: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	lines := bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte{'\n'})
	if got, want := len(lines), writers*perWriter; got != want {
		t.Fatalf("got %d lines, want %d", got, want)
	}
	for i, ln := range lines {
		if _, err := json.Marshal(decodeLine(t, ln)); err != nil {
			t.Errorf("line %d not valid JSON: %v", i, err)
		}
	}
}

func TestOpen_EmptyPath(t *testing.T) {
	_, err := Open("")
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}
