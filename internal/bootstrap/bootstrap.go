// Package bootstrap orchestrates the multi-phase worker provisioning pipeline.
//
// Each phase is a bash script under remote/ that the gateway scps onto the
// target worker and executes via SSH. The package:
//
//  1. Copies the phase script to the worker over SCP.
//  2. Runs it with a configurable timeout.
//  3. Parses the JSONL emitted on the script's stderr.
//  4. Returns a PhaseResult so the caller can aggregate progress and decide
//     whether to continue to the next phase.
//
// Phase 0 (remote/phase00_brew_baseline.sh) is the only phase bundled in
// this wave. Phases 1-9 arrive in WS-2 T2.4b once their bash fragments land.
//
// Dependency injection: the two side-effecting operations (SCP upload and SSH
// run) are modelled as function fields on Orchestrator so tests can substitute
// fakes without touching the network or ~/.ssh.
package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PhaseEvent is one JSONL line emitted by a phase script.
// Matches the canonical Owera schema: {ts,node,phase,action,result,duration_ms,stderr_tail}.
type PhaseEvent struct {
	Ts         string `json:"ts"`
	Node       string `json:"node"`
	Phase      string `json:"phase"`
	Action     string `json:"action"`
	Result     string `json:"result"`
	DurationMs int64  `json:"duration_ms"`
	StderrTail string `json:"stderr_tail,omitempty"`
}

// PhaseResult collects the outcome of one phase execution.
type PhaseResult struct {
	Phase     string
	ExitCode  int
	Events    []PhaseEvent
	ParseErrs []string // lines that were not valid JSONL
	Duration  time.Duration
}

// OK returns true if the phase exited 0 and no error events were emitted.
func (r PhaseResult) OK() bool {
	if r.ExitCode != 0 {
		return false
	}
	for _, e := range r.Events {
		if e.Result == "error" {
			return false
		}
	}
	return true
}

// Orchestrator runs bootstrap phases against a single worker target.
type Orchestrator struct {
	// ScriptDir is the local directory containing the phase bash scripts.
	// Defaults to "<repo-root>/remote" when empty. Tests pass a temp dir.
	ScriptDir string

	// Upload copies the script at localPath onto the worker at remotePath.
	// Signature matches a thin SCP wrapper.
	Upload func(ctx context.Context, localPath, remotePath string) error

	// Run executes remoteCmd on the worker and returns (stdout, stderr, exitCode, error).
	Run func(ctx context.Context, remoteCmd string) (stdout, stderr string, exitCode int, err error)

	// Now is used to measure phase duration. Defaults to time.Now.
	Now func() time.Time
}

// ErrNoScriptDir is returned when ScriptDir is empty and the default can't
// be resolved.
var ErrNoScriptDir = errors.New("bootstrap: script dir not set and could not be auto-detected")

// DefaultScriptDir resolves <repo-root>/remote from the current working
// directory. Works when the binary is run from within the repo; fails cleanly
// in detached deploys (where the operator must set ScriptDir explicitly).
func DefaultScriptDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("bootstrap: getwd: %w", err)
	}
	candidate := filepath.Join(wd, "remote")
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	}
	return "", ErrNoScriptDir
}

// RunPhase executes a single phase script:
//  1. Resolves the script path under ScriptDir.
//  2. Uploads it to /tmp/<name> on the worker.
//  3. Runs `bash /tmp/<name> <args...>` on the worker.
//  4. Parses JSONL events from stderr.
func (o *Orchestrator) RunPhase(ctx context.Context, scriptName string, args ...string) (PhaseResult, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	start := o.Now()

	scriptDir := o.ScriptDir
	if scriptDir == "" {
		var err error
		scriptDir, err = DefaultScriptDir()
		if err != nil {
			return PhaseResult{Phase: scriptName}, err
		}
	}

	localPath := filepath.Join(scriptDir, scriptName)
	if _, err := os.Stat(localPath); errors.Is(err, fs.ErrNotExist) {
		return PhaseResult{Phase: scriptName}, fmt.Errorf("bootstrap: script not found: %s", localPath)
	}

	remotePath := "/tmp/" + scriptName
	if err := o.Upload(ctx, localPath, remotePath); err != nil {
		return PhaseResult{Phase: scriptName}, fmt.Errorf("bootstrap: upload %s: %w", scriptName, err)
	}

	remoteCmd := buildRunCmd(remotePath, args)
	stdout, stderr, exit, err := o.Run(ctx, remoteCmd)
	dur := o.Now().Sub(start)

	res := PhaseResult{
		Phase:    scriptName,
		ExitCode: exit,
		Duration: dur,
	}
	if err != nil && exit == 0 {
		// Transport error (e.g., SSH session dropped). Treat as non-zero.
		res.ExitCode = -1
		res.ParseErrs = append(res.ParseErrs, fmt.Sprintf("transport error: %v", err))
	}

	res.Events, res.ParseErrs = parseEvents(stderr)
	_ = stdout
	return res, nil
}

func buildRunCmd(remotePath string, args []string) string {
	parts := []string{"bash", remotePath}
	parts = append(parts, args...)
	return strings.Join(parts, " ")
}

// parseEvents splits stderr on newlines and attempts to decode each line as
// a PhaseEvent. Lines that are not valid JSON are collected in ParseErrs.
func parseEvents(stderr string) (events []PhaseEvent, parseErrs []string) {
	for _, raw := range strings.Split(stderr, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "{") {
			// Non-JSON progress line (info/warn from the script's info() helper).
			continue
		}
		var ev PhaseEvent
		if err := json.NewDecoder(bytes.NewBufferString(line)).Decode(&ev); err != nil {
			parseErrs = append(parseErrs, fmt.Sprintf("parse error: %v (line: %s)", err, line))
			continue
		}
		events = append(events, ev)
	}
	return
}

// Summary returns a one-line human-readable description of the phase result.
func (r PhaseResult) Summary() string {
	status := "ok"
	if !r.OK() {
		status = fmt.Sprintf("exit=%d", r.ExitCode)
	}
	return fmt.Sprintf("%s: %s (%d events, %dms)",
		r.Phase, status, len(r.Events), r.Duration.Milliseconds())
}

// WriteReport writes a human-readable phase report to w.
func (r PhaseResult) WriteReport(w io.Writer) {
	fmt.Fprintf(w, "=== %s ===\n", r.Phase)
	fmt.Fprintf(w, "Exit: %d  Duration: %s\n", r.ExitCode, r.Duration.Round(time.Millisecond))
	for _, ev := range r.Events {
		tail := ""
		if ev.StderrTail != "" {
			tail = "  err=" + ev.StderrTail
		}
		fmt.Fprintf(w, "  [%s] %s → %s (%dms)%s\n", ev.Phase, ev.Action, ev.Result, ev.DurationMs, tail)
	}
	for _, pe := range r.ParseErrs {
		fmt.Fprintf(w, "  PARSE-ERR: %s\n", pe)
	}
}
