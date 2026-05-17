package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestWriteJSONAtomic_FreshFile verifies that writeJSONAtomic produces
// a valid file with the marshalled payload when no previous file
// exists.
func TestWriteJSONAtomic_FreshFile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "snap.json")

	payload := map[string]any{
		"ts":     "2026-05-17T12:00:00Z",
		"status": "operational",
	}
	if err := writeJSONAtomic(out, payload); err != nil {
		t.Fatalf("writeJSONAtomic: %v", err)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	if got["status"] != "operational" {
		t.Errorf("status: %v want operational", got["status"])
	}
}

// TestWriteJSONAtomic_OverwritesPrevious verifies that calling
// writeJSONAtomic a second time replaces the file contents wholesale.
// The atomic rename means a concurrent reader sees either the old
// complete file or the new complete file — never partial bytes.
func TestWriteJSONAtomic_OverwritesPrevious(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "snap.json")

	if err := writeJSONAtomic(out, map[string]any{"v": 1}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writeJSONAtomic(out, map[string]any{"v": 2, "extra": "y"}); err != nil {
		t.Fatalf("second write: %v", err)
	}

	raw, _ := os.ReadFile(out)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	if got["v"].(float64) != 2 {
		t.Errorf("v: %v want 2", got["v"])
	}
	if got["extra"] != "y" {
		t.Errorf("extra: %v want y", got["extra"])
	}

	// The tmp sibling should NOT exist post-rename.
	if _, err := os.Stat(out + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp left behind: %v", err)
	}
}

// TestWriteJSONAtomic_PrettyPrinted verifies the output is human-
// readable JSON (indented), which matters for operator debugging via
// `cat`. We don't lock the exact whitespace shape — just confirm
// MarshalIndent's newline/indent is present.
func TestWriteJSONAtomic_PrettyPrinted(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "snap.json")

	if err := writeJSONAtomic(out, map[string]any{"a": 1, "b": 2}); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw, _ := os.ReadFile(out)
	s := string(raw)
	if !strings.Contains(s, "\n") {
		t.Errorf("expected indented JSON with newlines; got %q", s)
	}
}

// TestWriteJSONAtomic_RejectsUnmarshallable ensures encoding errors
// surface from writeJSONAtomic without leaving a .tmp behind.
func TestWriteJSONAtomic_RejectsUnmarshallable(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "snap.json")

	// channels can't be JSON-marshalled.
	err := writeJSONAtomic(out, make(chan int))
	if err == nil {
		t.Fatal("expected error on unmarshallable input")
	}
	// No .tmp residue.
	if _, err := os.Stat(out + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp left behind on error: %v", err)
	}
	// No output file either.
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("out file created despite marshal error: %v", err)
	}
}

// --- HTTP PUT mode tests ---

func TestResolveHTTPPutConfig_FlagsOverEnv(t *testing.T) {
	t.Setenv(envSnapshotHTTPPutURL, "https://from-env.example/x")
	t.Setenv(envSnapshotHTTPAuth, "Bearer envtoken")

	cfg, err := resolveHTTPPutConfig("https://from-flag.example/x", []string{"X-Flag=1"}, 5*time.Second)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.URL != "https://from-flag.example/x" {
		t.Errorf("URL: got %q, want flag value", cfg.URL)
	}
	if cfg.Headers["X-Flag"] != "1" {
		t.Errorf("Headers: %+v", cfg.Headers)
	}
	if _, hasAuth := cfg.Headers["Authorization"]; hasAuth {
		t.Errorf("env auth leaked through despite flag presence: %+v", cfg.Headers)
	}
}

func TestResolveHTTPPutConfig_EnvFallback(t *testing.T) {
	t.Setenv(envSnapshotHTTPPutURL, "https://from-env.example/x")
	t.Setenv(envSnapshotHTTPAuth, "Bearer envtoken")
	cfg, err := resolveHTTPPutConfig("", nil, 5*time.Second)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.URL != "https://from-env.example/x" {
		t.Errorf("URL fallback: got %q", cfg.URL)
	}
	if cfg.Headers["Authorization"] != "Bearer envtoken" {
		t.Errorf("Authorization fallback: %+v", cfg.Headers)
	}
}

func TestResolveHTTPPutConfig_DisabledWhenNoURL(t *testing.T) {
	t.Setenv(envSnapshotHTTPPutURL, "")
	cfg, err := resolveHTTPPutConfig("", nil, 5*time.Second)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.Enabled() {
		t.Errorf("expected disabled when no URL configured; got %+v", cfg)
	}
}

func TestResolveHTTPPutConfig_BadHeaderFormat(t *testing.T) {
	if _, err := resolveHTTPPutConfig("https://x/y", []string{"NoEqualsHere"}, 5*time.Second); err == nil {
		t.Error("expected error on malformed header")
	}
}

// captureServer is a tiny test HTTP server that captures the most-
// recent PUT body + headers for assertion.
type captureServer struct {
	mu       sync.Mutex
	bodies   [][]byte
	headers  []http.Header
	respond  func() (int, time.Duration)
}

func newCaptureServer(t *testing.T, respond func() (int, time.Duration)) (*httptest.Server, *captureServer) {
	t.Helper()
	c := &captureServer{respond: respond}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, body)
		c.headers = append(c.headers, r.Header.Clone())
		c.mu.Unlock()
		status, sleep := http.StatusOK, time.Duration(0)
		if c.respond != nil {
			status, sleep = c.respond()
		}
		if sleep > 0 {
			time.Sleep(sleep)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func TestPutJSON_HappyPath(t *testing.T) {
	srv, cap := newCaptureServer(t, nil)
	body := []byte(`{"hello":"world"}`)
	if err := putJSON(context.Background(), srv.URL+"/x", body,
		map[string]string{"Authorization": "Bearer t"}, 2*time.Second); err != nil {
		t.Fatalf("putJSON: %v", err)
	}
	if len(cap.bodies) != 1 || !bytes.Equal(cap.bodies[0], body) {
		t.Errorf("body: got %v, want %s", cap.bodies, body)
	}
	if got := cap.headers[0].Get("Authorization"); got != "Bearer t" {
		t.Errorf("Authorization: got %q", got)
	}
	if got := cap.headers[0].Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type default: got %q", got)
	}
}

func TestPutJSON_Non2xxIsError(t *testing.T) {
	srv, _ := newCaptureServer(t, func() (int, time.Duration) { return http.StatusForbidden, 0 })
	if err := putJSON(context.Background(), srv.URL+"/x", []byte("{}"), nil, 2*time.Second); err == nil {
		t.Fatal("expected error on 403")
	}
}

func TestPutJSON_Timeout(t *testing.T) {
	srv, _ := newCaptureServer(t, func() (int, time.Duration) {
		return http.StatusOK, 200 * time.Millisecond
	})
	err := putJSON(context.Background(), srv.URL+"/x", []byte("{}"), nil, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestPutJSON_CustomContentTypeRespected(t *testing.T) {
	srv, cap := newCaptureServer(t, nil)
	err := putJSON(context.Background(), srv.URL+"/x", []byte("[]"),
		map[string]string{"Content-Type": "application/cbor"}, 2*time.Second)
	if err != nil {
		t.Fatalf("putJSON: %v", err)
	}
	if got := cap.headers[0].Get("Content-Type"); got != "application/cbor" {
		t.Errorf("Content-Type override: got %q", got)
	}
}
