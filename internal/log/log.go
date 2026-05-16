// Package log emits JSONL fleet-action events with the canonical Owera schema:
//
//	{ts, node, phase, action, result, duration_ms, stderr_tail}
//
// All fleetctl subcommands and remote/ bash fragments route their structured
// telemetry through this package so the fleet has a single, parseable event
// stream that downstream audit, metrics, and ledger consumers can rely on.
package log

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Result is the outcome category recorded in the JSONL `result` field.
//
// Use [ResultNoChange] for idempotent operations that detected the desired
// state was already present — this is load-bearing for re-running bootstrap
// phases without false failure signals.
type Result string

const (
	ResultOK       Result = "ok"
	ResultNoChange Result = "no-change"
	ResultError    Result = "error"
	ResultSkipped  Result = "skipped"
	ResultWarn     Result = "warn"
)

// Valid reports whether r is one of the known Result constants.
func (r Result) Valid() bool {
	switch r {
	case ResultOK, ResultNoChange, ResultError, ResultSkipped, ResultWarn:
		return true
	}
	return false
}

// Event is one JSONL line in the fleet log stream.
//
// Timestamp is serialized as RFC 3339 with nanosecond precision; the rest are
// plain strings/integers. StderrTail is omitted when empty so non-erroring
// events remain compact.
type Event struct {
	Timestamp  time.Time `json:"ts"`
	Node       string    `json:"node"`
	Phase      string    `json:"phase"`
	Action     string    `json:"action"`
	Result     Result    `json:"result"`
	DurationMs int64     `json:"duration_ms"`
	StderrTail string    `json:"stderr_tail,omitempty"`
}

// MaxStderrTailBytes caps the stderr capture written into an Event so a
// chatty failing process can't blow out the log line.
const MaxStderrTailBytes = 2048

// Logger writes JSONL events to an underlying io.Writer.
//
// Writes are serialized under a mutex so concurrent callers don't interleave
// partial JSON lines. A Logger is safe for use from multiple goroutines.
type Logger struct {
	mu     sync.Mutex
	w      io.Writer
	closer io.Closer
	now    func() time.Time
}

// New returns a Logger that writes to w. If w is also an io.Closer, calling
// [Logger.Close] will close it.
func New(w io.Writer) *Logger {
	l := &Logger{w: w, now: time.Now}
	if c, ok := w.(io.Closer); ok {
		l.closer = c
	}
	return l
}

// Open returns a Logger that appends JSONL events to the file at path.
// Parent directories are created with mode 0o755. The file is opened with
// mode 0o644 — sufficient for log inspection by the operator without
// exposing secrets (which must never appear in events).
func Open(path string) (*Logger, error) {
	if path == "" {
		return nil, errors.New("log: empty path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("log: mkdir %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("log: open %s: %w", path, err)
	}
	return New(f), nil
}

// Close releases the underlying writer if it implements io.Closer.
// Subsequent Action calls return [ErrClosed].
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closer == nil {
		return nil
	}
	err := l.closer.Close()
	l.closer = nil
	l.w = nil
	return err
}

// ErrClosed is returned from [Logger.Action] when the logger has been closed.
var ErrClosed = errors.New("log: logger closed")

// Action emits one JSONL line for e.
//
// If e.Timestamp is the zero value, the logger fills it with the current
// time (overridable in tests via [Logger.SetClock]). If e.Result is not one
// of the canonical constants, Action returns an error before writing.
// StderrTail is truncated to [MaxStderrTailBytes] from the tail (oldest
// bytes dropped, newest preserved) since the latest output usually carries
// the most diagnostic value.
func (l *Logger) Action(e Event) error {
	if !e.Result.Valid() {
		return fmt.Errorf("log: invalid Result %q", e.Result)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w == nil {
		return ErrClosed
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = l.now().UTC()
	}
	if n := len(e.StderrTail); n > MaxStderrTailBytes {
		e.StderrTail = e.StderrTail[n-MaxStderrTailBytes:]
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("log: marshal: %w", err)
	}
	if _, err := l.w.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("log: write: %w", err)
	}
	return nil
}

// SetClock overrides the timestamp source. Intended for tests; production
// callers should rely on the default time.Now.
func (l *Logger) SetClock(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}
