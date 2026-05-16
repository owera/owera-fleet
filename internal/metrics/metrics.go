// Package metrics exposes Prometheus-format metrics derived from the fleet's
// JSONL logs, ledger entries, and heartbeat state. The package builds the
// counter / gauge / histogram values from scanned log files (no agent running
// in-process — these are pull-time derivations) and renders them in the
// Prometheus text exposition format.
//
// The metrics surface is intentionally small (heartbeat age, request rate,
// per-task p50/p95 latency, delegate error rate). Adding a new metric is
// one entry in [Default].
package metrics

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Metric is one Prometheus metric line set.
type Metric struct {
	Name   string
	Help   string
	Type   string // "counter", "gauge", "histogram"
	Labels map[string]string
	Value  float64
}

// Collector builds metric values by scanning JSONL log files.
type Collector struct {
	mu      sync.Mutex
	LogDir  string // ~/.hermes/logs
	NodesFile string
	clock   func() time.Time
}

// New returns a Collector reading from logDir.
func New(logDir string) *Collector {
	return &Collector{LogDir: logDir, clock: time.Now}
}

// Collect scans the log directory and returns the current metric set.
func (c *Collector) Collect() ([]Metric, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var metrics []Metric

	// Heartbeat age per node.
	hb, err := c.collectHeartbeatAge()
	if err == nil {
		metrics = append(metrics, hb...)
	}

	// Per-phase counters and error rate.
	counters, errs := c.collectActionCounters()
	metrics = append(metrics, counters...)
	metrics = append(metrics, errs...)

	// Per-action latency p50/p95 over the last hour.
	lat := c.collectLatencyQuantiles()
	metrics = append(metrics, lat...)

	return metrics, nil
}

// Render writes the metrics in Prometheus text exposition format.
func Render(w io.Writer, metrics []Metric) error {
	// Group by metric name to emit HELP/TYPE once per name.
	grouped := make(map[string][]Metric)
	var order []string
	for _, m := range metrics {
		if _, ok := grouped[m.Name]; !ok {
			order = append(order, m.Name)
		}
		grouped[m.Name] = append(grouped[m.Name], m)
	}
	sort.Strings(order)

	for _, name := range order {
		group := grouped[name]
		first := group[0]
		if first.Help != "" {
			if _, err := fmt.Fprintf(w, "# HELP %s %s\n", name, first.Help); err != nil {
				return err
			}
		}
		if first.Type != "" {
			if _, err := fmt.Fprintf(w, "# TYPE %s %s\n", name, first.Type); err != nil {
				return err
			}
		}
		for _, m := range group {
			labels := formatLabels(m.Labels)
			if _, err := fmt.Fprintf(w, "%s%s %g\n", m.Name, labels, m.Value); err != nil {
				return err
			}
		}
	}
	return nil
}

// Serve starts an HTTP listener on addr that responds to GET /metrics with
// the current metric set. Returns the listener address (useful for ":0"
// bindings in tests) and a stop function.
func Serve(addr string, c *Collector) (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		metrics, err := c.Collect()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_ = Render(w, metrics)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.ListenAndServe() }()
	return srv, nil
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// collectHeartbeatAge reads ~/.hermes/heartbeats/*.json on the gateway to
// determine the age of each node's most recent heartbeat.
func (c *Collector) collectHeartbeatAge() ([]Metric, error) {
	heartbeatDir := filepath.Join(filepath.Dir(c.LogDir), "heartbeats")
	entries, err := os.ReadDir(heartbeatDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now := c.clock()
	var out []Metric
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, _ := e.Info()
		age := now.Sub(info.ModTime()).Seconds()
		node := strings.TrimSuffix(e.Name(), ".json")
		out = append(out, Metric{
			Name:   "fleet_heartbeat_age_seconds",
			Help:   "Seconds since the most recent heartbeat from each worker.",
			Type:   "gauge",
			Labels: map[string]string{"node": node},
			Value:  age,
		})
	}
	return out, nil
}

// collectActionCounters reads every *.jsonl in LogDir and produces:
//   - fleet_actions_total{phase,result}
//   - fleet_action_error_rate{phase} (over the last hour)
func (c *Collector) collectActionCounters() (counters []Metric, errRates []Metric) {
	entries, err := os.ReadDir(c.LogDir)
	if err != nil {
		return nil, nil
	}
	totals := make(map[string]map[string]int) // phase -> result -> count
	recentTotal := make(map[string]int)
	recentErrors := make(map[string]int)
	hourAgo := c.clock().Add(-time.Hour)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(c.LogDir, e.Name()))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var ev struct {
				Ts     time.Time `json:"ts"`
				Phase  string    `json:"phase"`
				Result string    `json:"result"`
			}
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				continue
			}
			if totals[ev.Phase] == nil {
				totals[ev.Phase] = map[string]int{}
			}
			totals[ev.Phase][ev.Result]++

			if ev.Ts.After(hourAgo) {
				recentTotal[ev.Phase]++
				if ev.Result == "error" {
					recentErrors[ev.Phase]++
				}
			}
		}
		f.Close()
	}

	for phase, byResult := range totals {
		for result, count := range byResult {
			counters = append(counters, Metric{
				Name:   "fleet_actions_total",
				Help:   "Total fleet actions by phase and result.",
				Type:   "counter",
				Labels: map[string]string{"phase": phase, "result": result},
				Value:  float64(count),
			})
		}
	}
	for phase, total := range recentTotal {
		if total == 0 {
			continue
		}
		rate := float64(recentErrors[phase]) / float64(total)
		errRates = append(errRates, Metric{
			Name:   "fleet_action_error_rate",
			Help:   "Error rate per phase over the last hour.",
			Type:   "gauge",
			Labels: map[string]string{"phase": phase},
			Value:  rate,
		})
	}
	return
}

// collectLatencyQuantiles computes p50/p95 duration_ms per phase.
func (c *Collector) collectLatencyQuantiles() []Metric {
	entries, err := os.ReadDir(c.LogDir)
	if err != nil {
		return nil
	}
	byPhase := make(map[string][]float64)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(c.LogDir, e.Name()))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var ev struct {
				Phase      string  `json:"phase"`
				DurationMs float64 `json:"duration_ms"`
			}
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				continue
			}
			if ev.DurationMs <= 0 {
				continue
			}
			byPhase[ev.Phase] = append(byPhase[ev.Phase], ev.DurationMs)
		}
		f.Close()
	}
	var out []Metric
	for phase, durations := range byPhase {
		if len(durations) == 0 {
			continue
		}
		sort.Float64s(durations)
		p50 := percentile(durations, 0.50)
		p95 := percentile(durations, 0.95)
		out = append(out, Metric{
			Name:   "fleet_action_duration_ms_p50",
			Help:   "50th percentile action duration in milliseconds, per phase.",
			Type:   "gauge",
			Labels: map[string]string{"phase": phase},
			Value:  p50,
		})
		out = append(out, Metric{
			Name:   "fleet_action_duration_ms_p95",
			Help:   "95th percentile action duration in milliseconds, per phase.",
			Type:   "gauge",
			Labels: map[string]string{"phase": phase},
			Value:  p95,
		})
	}
	return out
}

func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(q * float64(len(sorted)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
