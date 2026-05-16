// Package alerting routes operational alerts through a configurable backend:
// PagerDuty (pager-first), OpsGenie (pager-secondary), and ntfy.sh (chat).
// Backends are pluggable through the [Backend] interface so tests can capture
// alerts without hitting external services.
//
// The default routing policy ([Severity] → backends) is:
//
//	SeverityCritical → PagerDuty, OpsGenie, ntfy.sh
//	SeverityWarning  → ntfy.sh only
//	SeverityInfo     → ntfy.sh only (suppressible)
//
// Operators wire concrete backends at startup via [Router.AddBackend].
package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Severity is the alert severity level. Critical pages on-call; Warning and
// Info notify chat only.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

// Alert is one alert message.
type Alert struct {
	Severity   Severity          `json:"severity"`
	Title      string            `json:"title"`
	Body       string            `json:"body,omitempty"`
	Source     string            `json:"source,omitempty"` // e.g., "watchdog", "scenario"
	Dedup      string            `json:"dedup,omitempty"`  // dedup key for backends that support it
	Labels     map[string]string `json:"labels,omitempty"`
	OccurredAt time.Time         `json:"occurred_at,omitempty"`
}

// Backend delivers an alert through a specific channel.
type Backend interface {
	// Name returns a stable identifier (e.g., "pagerduty", "ntfy").
	Name() string

	// Send delivers the alert. Backends must return an error rather than panic
	// on transport failures so the router can record and continue.
	Send(ctx context.Context, alert Alert) error

	// HandlesSeverity reports whether this backend should be invoked for sev.
	HandlesSeverity(sev Severity) bool
}

// Router fans an alert out to every backend that accepts its severity.
type Router struct {
	backends []Backend
}

// NewRouter returns an empty Router.
func NewRouter() *Router {
	return &Router{}
}

// AddBackend registers a backend with the router. Backends are invoked in the
// order they are added.
func (r *Router) AddBackend(b Backend) {
	r.backends = append(r.backends, b)
}

// Fire delivers alert through every backend whose [Backend.HandlesSeverity]
// returns true. All backends are attempted; the first error is returned but
// every backend is still invoked.
func (r *Router) Fire(ctx context.Context, alert Alert) error {
	if alert.OccurredAt.IsZero() {
		alert.OccurredAt = time.Now().UTC()
	}
	var firstErr error
	for _, b := range r.backends {
		if !b.HandlesSeverity(alert.Severity) {
			continue
		}
		if err := b.Send(ctx, alert); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", b.Name(), err)
		}
	}
	return firstErr
}

// Backends returns the registered backend names (for diagnostics).
func (r *Router) Backends() []string {
	names := make([]string, 0, len(r.backends))
	for _, b := range r.backends {
		names = append(names, b.Name())
	}
	return names
}

// --------------------------------------------------------------------------
// PagerDuty (Events API v2)

// PagerDutyBackend fires events into the PagerDuty Events API v2.
type PagerDutyBackend struct {
	RoutingKey string
	Endpoint   string // override for tests; default is the real PagerDuty URL
	HTTPClient *http.Client
}

// NewPagerDuty returns a PagerDuty backend with the standard endpoint.
func NewPagerDuty(routingKey string) *PagerDutyBackend {
	return &PagerDutyBackend{
		RoutingKey: routingKey,
		Endpoint:   "https://events.pagerduty.com/v2/enqueue",
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Name implements Backend.
func (p *PagerDutyBackend) Name() string { return "pagerduty" }

// HandlesSeverity implements Backend — PagerDuty handles Critical only.
func (p *PagerDutyBackend) HandlesSeverity(s Severity) bool {
	return s == SeverityCritical
}

// Send implements Backend.
func (p *PagerDutyBackend) Send(ctx context.Context, alert Alert) error {
	body := map[string]any{
		"routing_key":  p.RoutingKey,
		"event_action": "trigger",
		"dedup_key":    alert.Dedup,
		"payload": map[string]any{
			"summary":   alert.Title,
			"severity":  pdSeverity(alert.Severity),
			"source":    alert.Source,
			"custom_details": map[string]any{
				"body":   alert.Body,
				"labels": alert.Labels,
			},
		},
	}
	return postJSON(ctx, p.HTTPClient, p.Endpoint, body)
}

func pdSeverity(s Severity) string {
	switch s {
	case SeverityCritical:
		return "critical"
	case SeverityWarning:
		return "warning"
	default:
		return "info"
	}
}

// --------------------------------------------------------------------------
// OpsGenie (Alerts API)

// OpsGenieBackend fires alerts into the OpsGenie Alerts API.
type OpsGenieBackend struct {
	APIKey     string
	Endpoint   string
	HTTPClient *http.Client
}

// NewOpsGenie returns an OpsGenie backend.
func NewOpsGenie(apiKey string) *OpsGenieBackend {
	return &OpsGenieBackend{
		APIKey:     apiKey,
		Endpoint:   "https://api.opsgenie.com/v2/alerts",
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Name implements Backend.
func (o *OpsGenieBackend) Name() string { return "opsgenie" }

// HandlesSeverity implements Backend — OpsGenie handles Critical only (pager).
func (o *OpsGenieBackend) HandlesSeverity(s Severity) bool {
	return s == SeverityCritical
}

// Send implements Backend.
func (o *OpsGenieBackend) Send(ctx context.Context, alert Alert) error {
	body := map[string]any{
		"message":     alert.Title,
		"description": alert.Body,
		"source":      alert.Source,
		"alias":       alert.Dedup,
		"priority":    "P1",
	}
	req, err := http.NewRequestWithContext(ctx, "POST", o.Endpoint, bytes.NewReader(mustJSON(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "GenieKey "+o.APIKey)
	req.Header.Set("Content-Type", "application/json")
	return doRequest(o.HTTPClient, req)
}

// --------------------------------------------------------------------------
// ntfy.sh

// NtfyBackend posts alerts to an ntfy.sh topic (self-hosted or hosted).
type NtfyBackend struct {
	Topic      string // full URL, e.g. "https://ntfy.sh/owera-fleet-alerts"
	HTTPClient *http.Client
	// Minimum severity to send. Defaults to SeverityInfo.
	MinSeverity Severity
}

// NewNtfy returns an ntfy backend for the given topic URL.
func NewNtfy(topicURL string) *NtfyBackend {
	return &NtfyBackend{
		Topic:       topicURL,
		HTTPClient:  &http.Client{Timeout: 10 * time.Second},
		MinSeverity: SeverityInfo,
	}
}

// Name implements Backend.
func (n *NtfyBackend) Name() string { return "ntfy" }

// HandlesSeverity implements Backend.
func (n *NtfyBackend) HandlesSeverity(s Severity) bool {
	return severityRank(s) >= severityRank(n.MinSeverity)
}

// Send implements Backend.
func (n *NtfyBackend) Send(ctx context.Context, alert Alert) error {
	if n.Topic == "" {
		return errors.New("ntfy: topic URL not set")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", n.Topic, bytes.NewReader([]byte(alert.Body)))
	if err != nil {
		return err
	}
	req.Header.Set("Title", alert.Title)
	req.Header.Set("Priority", ntfyPriority(alert.Severity))
	return doRequest(n.HTTPClient, req)
}

func ntfyPriority(s Severity) string {
	switch s {
	case SeverityCritical:
		return "urgent"
	case SeverityWarning:
		return "high"
	default:
		return "default"
	}
}

func severityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 3
	case SeverityWarning:
		return 2
	default:
		return 1
	}
}

// --------------------------------------------------------------------------
// HTTP helpers (shared)

func postJSON(ctx context.Context, client *http.Client, url string, body any) error {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(mustJSON(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return doRequest(client, req)
}

func doRequest(client *http.Client, req *http.Request) error {
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("http %d: %s", resp.StatusCode, string(body))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
