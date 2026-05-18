package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/owera/owera-fleet/internal/ledger"
	"github.com/owera/owera-fleet/internal/runregistry"
)

// TestSubmitJobRoundTrip exercises the contract that owera-cloud depends on:
//
//  1. POST a fleet.SubmitJob JSON-RPC envelope.
//  2. Get a task_id back.
//  3. Inspect the ledger for that task_id.
//  4. Assert every entry carries tenant_id from the original request.
//
// This is the integration test the acceptance criteria calls for.
//
// NOTE: depends on ledger.Entry.TenantID (added by parallel agent #5).
// Will not compile until that field lands.
func TestSubmitJobRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	ledgerDir := filepath.Join(tmp, "ledger")
	led, err := ledger.Open(ledgerDir)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}

	srv := NewServer("")
	h := NewSubmitJobHandler(led, ledgerDir)
	// Deterministic task_id for the assertion below.
	h.NewTaskID = func() string { return "task-test-12345678" }
	h.RegisterSKU("delegate.shell.v1", &ShellDelegateRouter{
		Ledger: led,
		Delegate: func(_ context.Context, node, cmd string) ([]ledger.Entry, error) {
			// Stand-in for the real ssh.Dial + client.Run. Returns one
			// successful entry; the router stamps tenant_id + task_id.
			return []ledger.Entry{{
				Phase:  "delegate",
				Action: "ssh:" + node + " :: " + cmd,
				Result: ledger.ResultOK,
			}}, nil
		},
	})
	srv.Register("fleet.SubmitJob", h.Handle)

	httpSrv := httptest.NewServer(srv.mux)
	defer httpSrv.Close()

	body := `{"jsonrpc":"2.0","method":"fleet.SubmitJob","params":{` +
		`"tenant_id":"acme-corp",` +
		`"sku":"delegate.shell.v1",` +
		`"cloud_job_id":"cloud-job-abc-123",` +
		`"inputs":{"node":"hermes@claw1.local","cmd":"uname -a"}` +
		`},"id":1}`

	resp := postRPC(t, httpSrv.URL, body)
	if resp.Error != nil {
		t.Fatalf("unexpected RPC error: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	taskID, _ := result["task_id"].(string)
	if taskID == "" {
		t.Fatalf("expected task_id in result, got %+v", result)
	}
	if taskID != "task-test-12345678" {
		t.Fatalf("expected deterministic task_id, got %q", taskID)
	}

	// Read every ledger entry for this task and assert tenant_id is
	// stamped on every single one. This is the contract.
	entries, err := led.Read(taskID)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("expected at least one ledger entry for %s", taskID)
	}
	for i, e := range entries {
		if e.TenantID != "acme-corp" {
			t.Fatalf("entry %d (phase=%s action=%s) missing tenant_id: got %q",
				i, e.Phase, e.Action, e.TenantID)
		}
	}
}

// TestSubmitJobIdempotency verifies the cloud_job_id dedup path. A second
// submission with the same cloud_job_id returns the original task_id
// without re-dispatching to the router.
func TestSubmitJobIdempotency(t *testing.T) {
	tmp := t.TempDir()
	ledgerDir := filepath.Join(tmp, "ledger")
	led, err := ledger.Open(ledgerDir)
	if err != nil {
		t.Fatal(err)
	}

	srv := NewServer("")
	h := NewSubmitJobHandler(led, ledgerDir)
	idCounter := 0
	h.NewTaskID = func() string {
		idCounter++
		return "task-iter-" + string(rune('0'+idCounter))
	}
	dispatchCount := 0
	h.RegisterSKU("delegate.shell.v1", SKURouterFunc(func(_ context.Context, _, _ string, _ map[string]interface{}) error {
		dispatchCount++
		return nil
	}))
	srv.Register("fleet.SubmitJob", h.Handle)

	httpSrv := httptest.NewServer(srv.mux)
	defer httpSrv.Close()

	body := `{"jsonrpc":"2.0","method":"fleet.SubmitJob","params":{` +
		`"tenant_id":"t1","sku":"delegate.shell.v1","cloud_job_id":"same-anchor",` +
		`"inputs":{"node":"x","cmd":"y"}},"id":1}`

	r1 := postRPC(t, httpSrv.URL, body)
	if r1.Error != nil {
		t.Fatalf("first call: %+v", r1.Error)
	}
	t1 := r1.Result.(map[string]any)["task_id"].(string)

	r2 := postRPC(t, httpSrv.URL, body)
	if r2.Error != nil {
		t.Fatalf("second call: %+v", r2.Error)
	}
	t2 := r2.Result.(map[string]any)["task_id"].(string)

	if t1 != t2 {
		t.Fatalf("expected same task_id on idempotent retry, got %q vs %q", t1, t2)
	}
	if dispatchCount != 1 {
		t.Fatalf("expected exactly 1 dispatch, got %d", dispatchCount)
	}
}

// TestSubmitJobUnknownSKU asserts the customer plane sees a structured
// error (not a 500) when it sends an unrecognized sku.
func TestSubmitJobUnknownSKU(t *testing.T) {
	tmp := t.TempDir()
	led, err := ledger.Open(filepath.Join(tmp, "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer("")
	h := NewSubmitJobHandler(led, filepath.Join(tmp, "ledger"))
	srv.Register("fleet.SubmitJob", h.Handle)

	httpSrv := httptest.NewServer(srv.mux)
	defer httpSrv.Close()

	body := `{"jsonrpc":"2.0","method":"fleet.SubmitJob","params":{` +
		`"tenant_id":"t1","sku":"unknown.future.v9","cloud_job_id":"cj-1",` +
		`"inputs":{}},"id":1}`
	r := postRPC(t, httpSrv.URL, body)
	if r.Error == nil {
		t.Fatal("expected error for unknown sku")
	}
	if r.Error.Code != CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %d", r.Error.Code)
	}
	if !strings.Contains(r.Error.Message, "unknown.future.v9") {
		t.Fatalf("expected sku name in error, got %q", r.Error.Message)
	}
}

// TestSubmitJobMissingFields covers each required-field branch.
func TestSubmitJobMissingFields(t *testing.T) {
	tmp := t.TempDir()
	led, _ := ledger.Open(filepath.Join(tmp, "ledger"))
	srv := NewServer("")
	h := NewSubmitJobHandler(led, filepath.Join(tmp, "ledger"))
	srv.Register("fleet.SubmitJob", h.Handle)
	httpSrv := httptest.NewServer(srv.mux)
	defer httpSrv.Close()

	cases := []struct{ name, body, wantSub string }{
		{"missing tenant", `{"jsonrpc":"2.0","method":"fleet.SubmitJob","params":{"sku":"x","cloud_job_id":"c"},"id":1}`, "tenant_id"},
		{"missing sku", `{"jsonrpc":"2.0","method":"fleet.SubmitJob","params":{"tenant_id":"t","cloud_job_id":"c"},"id":1}`, "sku"},
		{"missing cloud_job_id", `{"jsonrpc":"2.0","method":"fleet.SubmitJob","params":{"tenant_id":"t","sku":"x"},"id":1}`, "cloud_job_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := postRPC(t, httpSrv.URL, tc.body)
			if r.Error == nil || r.Error.Code != CodeInvalidParams {
				t.Fatalf("expected invalid params, got %+v", r.Error)
			}
			if !strings.Contains(r.Error.Message, tc.wantSub) {
				t.Fatalf("expected %q in message, got %q", tc.wantSub, r.Error.Message)
			}
		})
	}
}

// TestSubmitJobLongRunningRouter pins the H1 contract: a router that
// implements LongRunningRouter and returns true is NOT treated as
// finite. The handler must (a) not synthesise any terminal ledger
// entry, (b) keep the runregistry entry alive after Handle returns so
// fleet.CancelTask can find the task, and (c) still return the
// task_id + write the `submit` marker.
//
// The router itself in this test writes no ledger entries (mirroring
// the WS-A triage-watch stub which only emits a `bill` event via the
// BillingEmitter, not a terminal `complete`). After Dispatch returns,
// the ledger must contain ONLY the handler's `submit` marker — no
// `complete`, no `failed`, no `cancelled`.
func TestSubmitJobLongRunningRouter(t *testing.T) {
	tmp := t.TempDir()
	ledgerDir := filepath.Join(tmp, "ledger")
	led, err := ledger.Open(ledgerDir)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}

	srv := NewServer("")
	h := NewSubmitJobHandler(led, ledgerDir)
	h.NewTaskID = func() string { return "task-longrun-1" }

	// A LongRunningSKURouterFunc whose Dispatch returns nil immediately
	// (the cronjob would be installed here in the real triage-watch
	// path) and writes no ledger entries of its own.
	dispatched := 0
	h.RegisterSKU("triage-watch@v1", LongRunningSKURouterFunc(func(_ context.Context, _, _ string, _ map[string]interface{}) error {
		dispatched++
		return nil
	}))
	srv.Register("fleet.SubmitJob", h.Handle)

	httpSrv := httptest.NewServer(srv.mux)
	defer httpSrv.Close()

	body := `{"jsonrpc":"2.0","method":"fleet.SubmitJob","params":{` +
		`"tenant_id":"ten_acme",` +
		`"sku":"triage-watch@v1",` +
		`"cloud_job_id":"cloud-job-longrun-1",` +
		`"inputs":{"queue_url":"https://acme.zendesk.com","priority_threshold":8}` +
		`},"id":1}`

	resp := postRPC(t, httpSrv.URL, body)
	if resp.Error != nil {
		t.Fatalf("unexpected RPC error: %+v", resp.Error)
	}
	taskID, _ := resp.Result.(map[string]any)["task_id"].(string)
	if taskID != "task-longrun-1" {
		t.Fatalf("expected deterministic task_id task-longrun-1, got %q", taskID)
	}
	if dispatched != 1 {
		t.Fatalf("expected exactly 1 Dispatch call, got %d", dispatched)
	}

	// Ledger contract: exactly one `submit` marker, no terminal entry.
	entries, err := led.Read(taskID)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ledger entry count: got %d, want 1 (submit only); entries=%+v", len(entries), entries)
	}
	if entries[0].Phase != "submit" {
		t.Errorf("ledger[0].Phase: got %q, want %q", entries[0].Phase, "submit")
	}
	for _, e := range entries {
		if e.Phase == "complete" || e.Phase == "failed" || e.Phase == "cancelled" {
			t.Errorf("long-running handler must not synthesise terminal phase, got %q", e.Phase)
		}
	}

	// Runregistry contract: the task stays registered after Handle
	// returns so fleet.CancelTask can find it.
	if !runregistry.Has(taskID) {
		t.Errorf("runregistry: long-running task %q must remain registered after Handle returns", taskID)
	}

	// Cancellation should still be wired: cancelling via runregistry
	// invokes the dispatch ctx's cancel func and removes the entry.
	if !runregistry.Cancel(taskID) {
		t.Errorf("runregistry.Cancel(%q) returned false, expected true", taskID)
	}
	if runregistry.Has(taskID) {
		t.Errorf("runregistry: task %q should be unregistered after Cancel", taskID)
	}
}

// TestSubmitJobFiniteRouterUnregisters complements the long-running
// test: a router with no LongRunningRouter implementation is finite,
// so the handler must unregister it from runregistry as soon as
// Dispatch returns. Otherwise fleet.CancelTask would hang on tasks
// that have already terminated.
func TestSubmitJobFiniteRouterUnregisters(t *testing.T) {
	tmp := t.TempDir()
	ledgerDir := filepath.Join(tmp, "ledger")
	led, err := ledger.Open(ledgerDir)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}

	srv := NewServer("")
	h := NewSubmitJobHandler(led, ledgerDir)
	h.NewTaskID = func() string { return "task-finite-1" }
	h.RegisterSKU("delegate.shell.v1", SKURouterFunc(func(_ context.Context, _, _ string, _ map[string]interface{}) error {
		return nil
	}))
	srv.Register("fleet.SubmitJob", h.Handle)

	httpSrv := httptest.NewServer(srv.mux)
	defer httpSrv.Close()

	body := `{"jsonrpc":"2.0","method":"fleet.SubmitJob","params":{` +
		`"tenant_id":"ten_acme","sku":"delegate.shell.v1","cloud_job_id":"cj-finite-1",` +
		`"inputs":{"node":"x","cmd":"y"}},"id":1}`

	r := postRPC(t, httpSrv.URL, body)
	if r.Error != nil {
		t.Fatalf("unexpected RPC error: %+v", r.Error)
	}
	if runregistry.Has("task-finite-1") {
		t.Errorf("runregistry: finite task should be unregistered once Dispatch returns")
	}
}

// rawPost is a lighter test helper used where postRPC isn't enough.
// Kept here so this file is self-contained when read in isolation.
func rawPost(t *testing.T, url, body string) []byte {
	t.Helper()
	r, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	out, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// silenceUnused keeps rawPost + json import alive while we wait for #5
// to land. Once integration tests grow to need them, delete this.
var _ = rawPost
var _ = json.RawMessage(nil)
