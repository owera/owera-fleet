package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func resetReviewFlags() {
	reviewBranch = ""
	reviewOut = ""
	reviewJSON = false
	reviewQuiet = true
	reviewHermesDir = ""
}

func TestReviewFlagValidationNoBranch(t *testing.T) {
	resetReviewFlags()
	defer resetReviewFlags()

	// Override git to return an error so --branch detection fails.
	prev := gitRunFunc
	gitRunFunc = func(args ...string) (string, error) {
		return "", os.ErrNotExist
	}
	defer func() { gitRunFunc = prev }()

	err := runReview(reviewCmd, nil)
	if err == nil {
		t.Fatal("expected error when --branch and git both unavailable, got nil")
	}
	if !strings.Contains(err.Error(), "branch") {
		t.Errorf("error %q should mention 'branch'", err.Error())
	}
}

func TestReviewOutputFile(t *testing.T) {
	resetReviewFlags()
	defer resetReviewFlags()
	reviewBranch = "test-branch"

	// Stub git commands.
	prev := gitRunFunc
	gitRunFunc = func(args ...string) (string, error) {
		return "stub output", nil
	}
	defer func() { gitRunFunc = prev }()

	// Stub hermes runner.
	prevRunner := reviewRunner
	reviewRunner = func(_ context.Context, _ string) (string, error) {
		return "LGTM: no issues found.", nil
	}
	defer func() { reviewRunner = prevRunner }()

	// Point JSONL log at temp file.
	logDir := t.TempDir()
	prevLog := reviewLogPath
	reviewLogPath = filepath.Join(logDir, "review.jsonl")
	defer func() { reviewLogPath = prevLog }()

	// Write to file.
	outFile := filepath.Join(logDir, "review.md")
	reviewOut = outFile
	reviewQuiet = false
	defer func() { reviewOut = "" }()

	if err := runReview(reviewCmd, nil); err != nil {
		t.Fatalf("runReview: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !strings.HasPrefix(string(data), "# Code Review: test-branch") {
		t.Errorf("report header mismatch: %q", string(data)[:min(80, len(data))])
	}
	if !strings.Contains(string(data), "LGTM") {
		t.Error("report body missing hermes response")
	}
}

func TestReviewJSONLEmission(t *testing.T) {
	resetReviewFlags()
	defer resetReviewFlags()
	reviewBranch = "main"

	prev := gitRunFunc
	gitRunFunc = func(args ...string) (string, error) { return "", nil }
	defer func() { gitRunFunc = prev }()

	prevRunner := reviewRunner
	reviewRunner = func(_ context.Context, _ string) (string, error) {
		return "all good", nil
	}
	defer func() { reviewRunner = prevRunner }()

	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "review.jsonl")
	prevLog := reviewLogPath
	reviewLogPath = logPath
	defer func() { reviewLogPath = prevLog }()

	if err := runReview(reviewCmd, nil); err != nil {
		t.Fatalf("runReview: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var ev struct {
		Phase  string `json:"phase"`
		Action string `json:"action"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimRight(data, "\n"), &ev); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, data)
	}
	if ev.Phase != "review" {
		t.Errorf("phase = %q, want review", ev.Phase)
	}
	if !strings.HasPrefix(ev.Action, "review:") {
		t.Errorf("action = %q, expected review: prefix", ev.Action)
	}
	if ev.Result != "ok" {
		t.Errorf("result = %q, want ok", ev.Result)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestReviewStdout(t *testing.T) {
	resetReviewFlags()
	defer resetReviewFlags()
	reviewBranch = "feat/x"
	reviewQuiet = false

	prev := gitRunFunc
	gitRunFunc = func(args ...string) (string, error) { return "", nil }
	defer func() { gitRunFunc = prev }()

	prevRunner := reviewRunner
	reviewRunner = func(_ context.Context, _ string) (string, error) {
		return "No issues.", nil
	}
	defer func() { reviewRunner = prevRunner }()

	logDir := t.TempDir()
	prevLog := reviewLogPath
	reviewLogPath = filepath.Join(logDir, "review.jsonl")
	defer func() { reviewLogPath = prevLog }()

	var buf bytes.Buffer
	reviewCmd.SetOut(&buf)
	defer reviewCmd.SetOut(nil)

	if err := runReview(reviewCmd, nil); err != nil {
		t.Fatalf("runReview: %v", err)
	}
	if !strings.Contains(buf.String(), "feat/x") {
		t.Errorf("stdout missing branch name: %s", buf.String())
	}
}
