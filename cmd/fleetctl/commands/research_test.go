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

func resetResearchFlags() {
	researchPrompt = ""
	researchOut = ""
	researchWeb = false
	researchJSON = false
	researchQuiet = true
	researchHermesDir = ""
}

func TestResearchRequiresPrompt(t *testing.T) {
	resetResearchFlags()
	defer resetResearchFlags()

	err := runResearch(researchCmd, nil)
	if err == nil {
		t.Fatal("expected error for missing --prompt, got nil")
	}
	if !strings.Contains(err.Error(), "--prompt") {
		t.Errorf("error %q should mention '--prompt'", err.Error())
	}
}

func TestResearchJSONLEmission(t *testing.T) {
	resetResearchFlags()
	defer resetResearchFlags()
	researchPrompt = "What is Go?"

	prevRunner := researchRunner
	researchRunner = func(_ context.Context, _ string, _ bool) (string, error) {
		return "Go is a statically typed language.", nil
	}
	defer func() { researchRunner = prevRunner }()

	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "research.jsonl")
	prevLog := researchLogPath
	researchLogPath = logPath
	defer func() { researchLogPath = prevLog }()

	if err := runResearch(researchCmd, nil); err != nil {
		t.Fatalf("runResearch: %v", err)
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
	if ev.Phase != "research" {
		t.Errorf("phase = %q, want research", ev.Phase)
	}
	if !strings.HasPrefix(ev.Action, "research:") {
		t.Errorf("action = %q, expected research: prefix", ev.Action)
	}
	if ev.Result != "ok" {
		t.Errorf("result = %q, want ok", ev.Result)
	}
}

func TestResearchOutputFile(t *testing.T) {
	resetResearchFlags()
	defer resetResearchFlags()
	researchPrompt = "Test research question"
	researchQuiet = false

	prevRunner := researchRunner
	researchRunner = func(_ context.Context, _ string, _ bool) (string, error) {
		return "Answer to your question.", nil
	}
	defer func() { researchRunner = prevRunner }()

	logDir := t.TempDir()
	prevLog := researchLogPath
	researchLogPath = filepath.Join(logDir, "research.jsonl")
	defer func() { researchLogPath = prevLog }()

	outFile := filepath.Join(logDir, "result.md")
	researchOut = outFile
	defer func() { researchOut = "" }()

	if err := runResearch(researchCmd, nil); err != nil {
		t.Fatalf("runResearch: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !strings.HasPrefix(string(data), "# Research:") {
		t.Errorf("report header mismatch: %q", string(data)[:min(60, len(data))])
	}
	if !strings.Contains(string(data), "Answer to your question.") {
		t.Error("response body missing from report")
	}
}
