package metrics_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/owera/owera-fleet/internal/metrics"
)

func TestCollectFromJSONL(t *testing.T) {
	tmp := t.TempDir()
	logDir := filepath.Join(tmp, "logs")
	_ = os.MkdirAll(logDir, 0o755)

	now := time.Now().UTC().Format(time.RFC3339)
	lines := []string{
		`{"ts":"` + now + `","phase":"delegate","action":"a","result":"ok","duration_ms":100}`,
		`{"ts":"` + now + `","phase":"delegate","action":"b","result":"error","duration_ms":200}`,
		`{"ts":"` + now + `","phase":"health","action":"c","result":"ok","duration_ms":50}`,
	}
	if err := os.WriteFile(filepath.Join(logDir, "test.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := metrics.New(logDir)
	out, err := c.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	// Expect counters for both phases + error rate for delegate + latency p50/p95.
	foundCounter := false
	foundErrRate := false
	foundP95 := false
	for _, m := range out {
		if m.Name == "fleet_actions_total" && m.Labels["phase"] == "delegate" {
			foundCounter = true
		}
		if m.Name == "fleet_action_error_rate" && m.Labels["phase"] == "delegate" {
			foundErrRate = true
			if m.Value < 0.4 || m.Value > 0.6 {
				t.Errorf("error_rate = %f, want ~0.5", m.Value)
			}
		}
		if m.Name == "fleet_action_duration_ms_p95" {
			foundP95 = true
		}
	}
	if !foundCounter {
		t.Error("missing fleet_actions_total counter")
	}
	if !foundErrRate {
		t.Error("missing fleet_action_error_rate")
	}
	if !foundP95 {
		t.Error("missing fleet_action_duration_ms_p95")
	}
}

func TestRenderProm(t *testing.T) {
	ms := []metrics.Metric{
		{Name: "fleet_test_gauge", Help: "test", Type: "gauge", Labels: map[string]string{"node": "n1"}, Value: 3.14},
		{Name: "fleet_test_gauge", Help: "test", Type: "gauge", Labels: map[string]string{"node": "n2"}, Value: 2.71},
	}
	var buf bytes.Buffer
	if err := metrics.Render(&buf, ms); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "# TYPE fleet_test_gauge gauge") {
		t.Errorf("missing TYPE line: %s", out)
	}
	if !strings.Contains(out, `fleet_test_gauge{node="n1"} 3.14`) {
		t.Errorf("missing n1 metric line: %s", out)
	}
}

func TestServe(t *testing.T) {
	tmp := t.TempDir()
	_ = os.MkdirAll(filepath.Join(tmp, "logs"), 0o755)
	c := metrics.New(filepath.Join(tmp, "logs"))
	srv, err := metrics.Serve("127.0.0.1:0", c)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer srv.Close()
	// We don't actually hit it in this unit test — just confirm we created the server cleanly.
}
