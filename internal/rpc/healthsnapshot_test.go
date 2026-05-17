package rpc_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/owera/owera-fleet/internal/ledger"
	"github.com/owera/owera-fleet/internal/nodes"
	"github.com/owera/owera-fleet/internal/rpc"
)

// cloudSnapshot mirrors the cloud-side `api/internal/status/Snapshot`
// Go struct that consumes this RPC. The shape is contract-frozen; the
// test fails if a field name or JSON tag in the handler drifts.
//
// Keep this struct in lockstep with owera-cloud's internal/status
// package. The acceptance criteria for issue #4 require a passing
// decode here.
type cloudSnapshot struct {
	Ts      string `json:"ts"`
	Gateway struct {
		OK            bool   `json:"ok"`
		HermesVersion string `json:"hermes_version"`
		UptimeSeconds int64  `json:"uptime_seconds"`
	} `json:"gateway"`
	Workers []struct {
		Node                    string  `json:"node"`
		OK                      bool    `json:"ok"`
		LastHeartbeatAgeSeconds float64 `json:"last_heartbeat_age_seconds"`
		HermesVersion           string  `json:"hermes_version"`
	} `json:"workers"`
	SKUConformance []struct {
		SKU          string  `json:"sku"`
		P50LatencyMs float64 `json:"p50_latency_ms"`
		P95LatencyMs float64 `json:"p95_latency_ms"`
		ErrorRatePct float64 `json:"error_rate_pct"`
		SLATargetMs  int64   `json:"sla_target_ms"`
	} `json:"sku_conformance"`
}

// fakeDeps builds a fully synthetic Deps so tests never touch the
// filesystem or shell out. The atomic counters let the cache test
// assert that the underlying scan only ran once.
func fakeDeps(now func() time.Time) (*rpc.Deps, *int64, *int64) {
	var heartbeatCalls, versionCalls int64
	d := &rpc.Deps{
		Now:          now,
		GatewayStart: now().Add(-2 * time.Minute),
		HeartbeatAges: func() (map[string]time.Duration, error) {
			atomic.AddInt64(&heartbeatCalls, 1)
			return map[string]time.Duration{
				"claw1.local": 12 * time.Second,
				"claw2.local": 8 * time.Minute, // unhealthy: > 5 min threshold
			}, nil
		},
		LoadNodes: func() ([]nodes.Node, error) {
			return []nodes.Node{
				{User: "hermes", Host: "claw1.local"},
				{User: "hermes", Host: "claw2.local"},
			}, nil
		},
		HermesVersion: func(context.Context) (string, error) {
			atomic.AddInt64(&versionCalls, 1)
			return "v0.13.0", nil
		},
		LedgerTasks: func() ([]string, error) { return nil, nil },
		LedgerRead:  func(string) ([]ledger.Entry, error) { return nil, nil },
	}
	return d, &heartbeatCalls, &versionCalls
}

func TestSnapshot_Shape(t *testing.T) {
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	deps, _, _ := fakeDeps(func() time.Time { return now })
	h := rpc.NewHealthSnapshotHandler(deps)

	snap, err := h.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// Round-trip the snapshot through JSON to verify the shape
	// matches the cloud-side struct exactly. If any JSON tag drifts
	// (e.g. uptime_seconds → uptimeSeconds) the decode below fails.
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var cloud cloudSnapshot
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cloud); err != nil {
		t.Fatalf("decode into cloudSnapshot: %v\npayload: %s", err, raw)
	}

	if cloud.Ts == "" {
		t.Error("ts is empty")
	}
	if !cloud.Gateway.OK {
		t.Error("gateway.ok = false, want true")
	}
	if cloud.Gateway.HermesVersion != "v0.13.0" {
		t.Errorf("gateway.hermes_version = %q, want v0.13.0", cloud.Gateway.HermesVersion)
	}
	if cloud.Gateway.UptimeSeconds < 100 || cloud.Gateway.UptimeSeconds > 200 {
		t.Errorf("gateway.uptime_seconds = %d, want ~120", cloud.Gateway.UptimeSeconds)
	}
	if len(cloud.Workers) != 2 {
		t.Fatalf("workers len = %d, want 2", len(cloud.Workers))
	}

	byNode := map[string]struct {
		ok  bool
		age float64
	}{}
	for _, w := range cloud.Workers {
		byNode[w.Node] = struct {
			ok  bool
			age float64
		}{w.OK, w.LastHeartbeatAgeSeconds}
	}
	if !byNode["claw1.local"].ok {
		t.Error("claw1.local should be ok (12s < 5min)")
	}
	if byNode["claw2.local"].ok {
		t.Error("claw2.local should NOT be ok (8min > 5min)")
	}
	if byNode["claw1.local"].age != 12 {
		t.Errorf("claw1.local age = %v, want 12", byNode["claw1.local"].age)
	}

	// SKU conformance is empty in v1 but must marshal as [] not null
	// so the cloud status page doesn't show "null" in the JSON.
	if cloud.SKUConformance == nil {
		t.Error("sku_conformance is nil; should be empty slice for JSON []")
	}
	if !strings.Contains(string(raw), `"sku_conformance":[]`) {
		t.Errorf("sku_conformance not marshaled as []: %s", raw)
	}

	// Ts must parse as RFC3339.
	if _, err := time.Parse(time.RFC3339, cloud.Ts); err != nil {
		t.Errorf("ts %q not RFC3339: %v", cloud.Ts, err)
	}
}

func TestSnapshot_CacheSingleFlight(t *testing.T) {
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	clock := now
	deps, heartbeatCalls, _ := fakeDeps(func() time.Time { return clock })
	h := rpc.NewHealthSnapshotHandler(deps)

	// First call populates the cache.
	if _, err := h.Snapshot(context.Background()); err != nil {
		t.Fatalf("first Snapshot: %v", err)
	}
	if got := atomic.LoadInt64(heartbeatCalls); got != 1 {
		t.Fatalf("after first call heartbeatCalls = %d, want 1", got)
	}

	// Second call within TTL must NOT trigger a re-scan.
	clock = clock.Add(10 * time.Second)
	if _, err := h.Snapshot(context.Background()); err != nil {
		t.Fatalf("second Snapshot: %v", err)
	}
	if got := atomic.LoadInt64(heartbeatCalls); got != 1 {
		t.Errorf("after cached call heartbeatCalls = %d, want still 1", got)
	}

	// Third call also within TTL.
	clock = clock.Add(15 * time.Second)
	if _, err := h.Snapshot(context.Background()); err != nil {
		t.Fatalf("third Snapshot: %v", err)
	}
	if got := atomic.LoadInt64(heartbeatCalls); got != 1 {
		t.Errorf("after second cached call heartbeatCalls = %d, want still 1", got)
	}

	// Past TTL: a new scan must happen.
	clock = clock.Add(10 * time.Second) // total elapsed: 35s > 30s
	if _, err := h.Snapshot(context.Background()); err != nil {
		t.Fatalf("fourth Snapshot: %v", err)
	}
	if got := atomic.LoadInt64(heartbeatCalls); got != 2 {
		t.Errorf("after TTL-expired call heartbeatCalls = %d, want 2", got)
	}
}

func TestSnapshot_ConcurrentCallersSingleFlight(t *testing.T) {
	// 100 goroutines racing into a cold cache: only one underlying
	// scan should fire. This proves the single-flight refresh works
	// (the "sync.Once-style" requirement in the acceptance criteria).
	now := time.Now()
	deps, heartbeatCalls, _ := fakeDeps(func() time.Time { return now })

	// Inject artificial latency into the scan so the race window is
	// guaranteed to overlap across goroutines.
	original := deps.HeartbeatAges
	deps.HeartbeatAges = func() (map[string]time.Duration, error) {
		time.Sleep(20 * time.Millisecond)
		return original()
	}

	h := rpc.NewHealthSnapshotHandler(deps)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.Snapshot(context.Background()); err != nil {
				t.Errorf("Snapshot: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(heartbeatCalls); got != 1 {
		t.Errorf("concurrent callers triggered %d scans, want 1", got)
	}
}

func TestSnapshot_RegistryDriftSurfaces(t *testing.T) {
	// A worker reporting heartbeats but not in nodes.txt should still
	// appear in the snapshot (so operators see the drift instead of
	// silently dropping the rogue worker).
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	deps, _, _ := fakeDeps(func() time.Time { return now })
	deps.LoadNodes = func() ([]nodes.Node, error) {
		return []nodes.Node{{User: "hermes", Host: "claw1.local"}}, nil
	}
	// fakeDeps' HeartbeatAges still includes claw2.local; it should
	// surface as a heartbeat-only worker.
	h := rpc.NewHealthSnapshotHandler(deps)
	snap, err := h.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Workers) != 2 {
		t.Fatalf("workers len = %d, want 2 (registry + drift)", len(snap.Workers))
	}
	if snap.Workers[0].Node != "claw1.local" {
		t.Errorf("workers[0] = %q, want claw1.local (registry order first)", snap.Workers[0].Node)
	}
	if snap.Workers[1].Node != "claw2.local" {
		t.Errorf("workers[1] = %q, want claw2.local (drift appended)", snap.Workers[1].Node)
	}
}

func TestSnapshot_RegistryOnlyMissingHeartbeat(t *testing.T) {
	// Worker in registry with no heartbeat yet: ok=false, age=-1.
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	deps, _, _ := fakeDeps(func() time.Time { return now })
	deps.HeartbeatAges = func() (map[string]time.Duration, error) {
		return map[string]time.Duration{"claw1.local": 5 * time.Second}, nil
	}
	deps.LoadNodes = func() ([]nodes.Node, error) {
		return []nodes.Node{
			{User: "hermes", Host: "claw1.local"},
			{User: "hermes", Host: "claw99.local"},
		}, nil
	}
	h := rpc.NewHealthSnapshotHandler(deps)
	snap, err := h.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	var missing *rpc.WorkerHealth
	for i := range snap.Workers {
		if snap.Workers[i].Node == "claw99.local" {
			missing = &snap.Workers[i]
		}
	}
	if missing == nil {
		t.Fatal("claw99.local not in snapshot")
	}
	if missing.OK {
		t.Error("claw99.local ok = true, want false (no heartbeat)")
	}
	if missing.LastHeartbeatAgeSeconds != -1 {
		t.Errorf("claw99.local age = %v, want -1 sentinel", missing.LastHeartbeatAgeSeconds)
	}
}

func TestSnapshot_GatewayUnhealthyOnVersionFailure(t *testing.T) {
	// If hermes --version fails, gateway.ok must be false so /readyz
	// can use the flag as a 500 trigger.
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	deps, _, _ := fakeDeps(func() time.Time { return now })
	deps.HermesVersion = func(context.Context) (string, error) {
		return "", context.DeadlineExceeded
	}
	h := rpc.NewHealthSnapshotHandler(deps)
	snap, err := h.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Gateway.OK {
		t.Error("gateway.ok = true on version failure; want false")
	}
}

// stubRegistrar captures Register calls so we can verify the handler
// installs itself under the contract method name. The signature
// matches what agent #2's JSON-RPC server is expected to expose.
type stubRegistrar struct {
	registered map[string]func(ctx context.Context, params json.RawMessage) (any, error)
}

func (s *stubRegistrar) Register(method string, h func(ctx context.Context, params json.RawMessage) (any, error)) {
	if s.registered == nil {
		s.registered = make(map[string]func(ctx context.Context, params json.RawMessage) (any, error))
	}
	s.registered[method] = h
}

func TestHandler_RegistersCanonicalMethod(t *testing.T) {
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	deps, _, _ := fakeDeps(func() time.Time { return now })
	h := rpc.NewHealthSnapshotHandler(deps)

	reg := &stubRegistrar{}
	h.Register(reg)

	fn, ok := reg.registered["fleet.HealthSnapshot"]
	if !ok {
		t.Fatalf("handler did not register under fleet.HealthSnapshot; got %v", keys(reg.registered))
	}

	// The registered function must dispatch through the cache.
	res, err := fn(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("registered handler errored: %v", err)
	}
	if _, ok := res.(*rpc.HealthSnapshot); !ok {
		t.Errorf("registered handler returned %T, want *rpc.HealthSnapshot", res)
	}
}

func keys(m map[string]func(ctx context.Context, params json.RawMessage) (any, error)) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestRefresh_BypassesCache(t *testing.T) {
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	deps, heartbeatCalls, _ := fakeDeps(func() time.Time { return now })
	h := rpc.NewHealthSnapshotHandler(deps)

	if _, err := h.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := h.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := atomic.LoadInt64(heartbeatCalls); got != 2 {
		t.Errorf("after Refresh, heartbeatCalls = %d, want 2", got)
	}
}
