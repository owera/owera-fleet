package alerting_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/owera/owera-fleet/internal/alerting"
)

type captureBackend struct {
	mu          sync.Mutex
	name        string
	handles     alerting.Severity
	calls       []alerting.Alert
	err         error
}

func (c *captureBackend) Name() string { return c.name }
func (c *captureBackend) HandlesSeverity(s alerting.Severity) bool {
	return s == c.handles
}
func (c *captureBackend) Send(_ context.Context, a alerting.Alert) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, a)
	return c.err
}

func TestRouterFanout(t *testing.T) {
	r := alerting.NewRouter()
	crit := &captureBackend{name: "pager", handles: alerting.SeverityCritical}
	chat := &captureBackend{name: "chat", handles: alerting.SeverityWarning}
	r.AddBackend(crit)
	r.AddBackend(chat)

	if err := r.Fire(context.Background(), alerting.Alert{Severity: alerting.SeverityCritical, Title: "down"}); err != nil {
		t.Fatalf("Fire critical: %v", err)
	}
	if len(crit.calls) != 1 {
		t.Errorf("pager calls = %d, want 1", len(crit.calls))
	}
	if len(chat.calls) != 0 {
		t.Errorf("chat got critical alert, len=%d", len(chat.calls))
	}

	_ = r.Fire(context.Background(), alerting.Alert{Severity: alerting.SeverityWarning, Title: "slow"})
	if len(chat.calls) != 1 {
		t.Errorf("chat calls = %d, want 1", len(chat.calls))
	}
}

func TestRouterReturnsFirstError(t *testing.T) {
	r := alerting.NewRouter()
	failing := &captureBackend{name: "fail", handles: alerting.SeverityCritical, err: errors.New("boom")}
	working := &captureBackend{name: "ok", handles: alerting.SeverityCritical}
	r.AddBackend(failing)
	r.AddBackend(working)

	err := r.Fire(context.Background(), alerting.Alert{Severity: alerting.SeverityCritical, Title: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	// Working backend must still be invoked.
	if len(working.calls) != 1 {
		t.Errorf("working got %d calls, want 1", len(working.calls))
	}
}

func TestPagerDutyHTTP(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody = make([]byte, r.ContentLength)
		_, _ = r.Body.Read(gotBody)
		w.WriteHeader(202)
	}))
	defer srv.Close()

	pd := alerting.NewPagerDuty("test-routing-key")
	pd.Endpoint = srv.URL
	pd.HTTPClient = srv.Client()

	if !pd.HandlesSeverity(alerting.SeverityCritical) {
		t.Error("PagerDuty should handle Critical")
	}
	if pd.HandlesSeverity(alerting.SeverityWarning) {
		t.Error("PagerDuty should NOT handle Warning")
	}
	if err := pd.Send(context.Background(), alerting.Alert{
		Severity: alerting.SeverityCritical,
		Title:    "test",
		Dedup:    "k1",
		Source:   "test",
	}); err != nil {
		t.Errorf("Send: %v", err)
	}
}

func TestNtfyHTTP(t *testing.T) {
	var gotTitle string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTitle = r.Header.Get("Title")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	ntfy := alerting.NewNtfy(srv.URL)
	ntfy.HTTPClient = srv.Client()
	if err := ntfy.Send(context.Background(), alerting.Alert{Title: "hello", Body: "world"}); err != nil {
		t.Errorf("Send: %v", err)
	}
	if gotTitle != "hello" {
		t.Errorf("title = %q", gotTitle)
	}
}

func TestNtfyMinSeverity(t *testing.T) {
	ntfy := alerting.NewNtfy("http://example/test")
	ntfy.MinSeverity = alerting.SeverityWarning
	if ntfy.HandlesSeverity(alerting.SeverityInfo) {
		t.Error("ntfy should not handle Info with MinSeverity=Warning")
	}
	if !ntfy.HandlesSeverity(alerting.SeverityWarning) {
		t.Error("ntfy should handle Warning")
	}
}
