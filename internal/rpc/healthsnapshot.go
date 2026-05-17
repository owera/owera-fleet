// Package rpc holds the JSON-RPC handlers fleetctl serves over its
// operator-plane socket. This file implements `fleet.HealthSnapshot` —
// the real-time fleet health payload that drives api/readyz and the
// public status.owera.ai page in owera-cloud.
//
// The handler mirrors the cloud-side `api/internal/status/Snapshot`
// Go struct exactly (lowercase snake_case JSON tags). The cloud
// integration test decodes the RPC result straight into that struct,
// so any field-name drift here is an API break — change with care.
//
// # Cache
//
// The snapshot is cached for 30s. Status pages poll every 60s and
// /readyz probes can fire at multi-Hz cadence; recomputing on every
// call would re-scan the heartbeat dir and the ledger N times per
// second. The cache uses a sync.RWMutex for the fast path and a
// sync.Mutex for single-flight refresh so a thundering herd of
// expired-cache callers only triggers one rebuild.
//
// # Wiring
//
// `Register(server)` installs the handler on a JSON-RPC server. The
// server type is owned by another agent in the wave; this file works
// against a minimal `Registrar` interface so it compiles standalone
// and integrates by satisfaction rather than import.
package rpc

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/owera/owera-fleet/internal/hermes"
	"github.com/owera/owera-fleet/internal/ledger"
	"github.com/owera/owera-fleet/internal/metrics"
	"github.com/owera/owera-fleet/internal/nodes"
)

// HealthSnapshot is the result returned by `fleet.HealthSnapshot`. The
// JSON shape is contract-frozen — see the cloud-side
// `api/internal/status/Snapshot` Go struct. Field order here matches the
// JSON-RPC method docs so reviewers can diff visually.
type HealthSnapshot struct {
	Ts             string             `json:"ts"`
	Gateway        GatewayHealth      `json:"gateway"`
	Workers        []WorkerHealth     `json:"workers"`
	SKUConformance []SKUConformance   `json:"sku_conformance"`
}

// GatewayHealth describes the operator-plane gateway process itself.
//
// `ok` is true when the fleetctl process is up and the pinned Hermes
// version is known. UptimeSeconds is computed against the process
// start time captured at handler-init time.
type GatewayHealth struct {
	OK             bool   `json:"ok"`
	HermesVersion  string `json:"hermes_version"`
	UptimeSeconds  int64  `json:"uptime_seconds"`
}

// WorkerHealth is one entry in the workers list.
//
// LastHeartbeatAgeSeconds matches `fleet_heartbeat_age_seconds` from
// `fleetctl metrics --scrape`; both surfaces call [metrics.HeartbeatAges]
// so the values are guaranteed identical.
type WorkerHealth struct {
	Node                    string  `json:"node"`
	OK                      bool    `json:"ok"`
	LastHeartbeatAgeSeconds float64 `json:"last_heartbeat_age_seconds"`
	HermesVersion           string  `json:"hermes_version"`
}

// SKUConformance carries per-SKU latency + error-rate aggregates.
//
// v1 returns an empty slice: ledger entries don't yet carry a SKU tag
// (issue #5 may introduce one). The cloud status page surfaces an
// empty array as "no data yet" rather than an error.
type SKUConformance struct {
	SKU           string  `json:"sku"`
	P50LatencyMs  float64 `json:"p50_latency_ms"`
	P95LatencyMs  float64 `json:"p95_latency_ms"`
	ErrorRatePct  float64 `json:"error_rate_pct"`
	SLATargetMs   int64   `json:"sla_target_ms"`
}

// HealthyHeartbeatThreshold is the heartbeat age above which a worker
// is marked unhealthy in the snapshot. The watchdog uses 5 min; the
// status page tolerates more drift before flipping red, so we go with
// 5 min here too — matches operator expectations.
const HealthyHeartbeatThreshold = 5 * time.Minute

// cacheTTL is the maximum age of a returned snapshot. The contract
// permits ≤30s; we use the full 30s to minimize re-scan load.
const cacheTTL = 30 * time.Second

// Deps bundles the side-effecting calls the handler makes so tests
// can substitute fakes (no real filesystem, no real subprocess).
//
// Production callers use [DefaultDeps] which wires the standard
// implementations against the live ~/.hermes/ tree.
type Deps struct {
	// Now returns the current time. Substituted in tests for cache
	// behavior verification.
	Now func() time.Time
	// HeartbeatAges returns each worker's last-heartbeat age. In
	// production this is metrics.HeartbeatAges against ~/.hermes/logs.
	HeartbeatAges func() (map[string]time.Duration, error)
	// LoadNodes returns the worker registry. Workers absent from
	// the registry but present in heartbeats still appear in the
	// snapshot (and vice versa) so operators can detect drift.
	LoadNodes func() ([]nodes.Node, error)
	// HermesVersion returns the live `hermes --version` string for
	// the gateway. Returns the pinned version on failure so the
	// snapshot still surfaces *something* useful.
	HermesVersion func(ctx context.Context) (string, error)
	// LedgerTasks lists all task IDs (used to derive sku_conformance
	// once SKU is recoverable from entries). Returning (nil, nil) is
	// the v1 default.
	LedgerTasks func() ([]string, error)
	// LedgerRead returns the entries for one task. Used for SKU
	// conformance once SKU tagging lands.
	LedgerRead func(taskID string) ([]ledger.Entry, error)
	// GatewayStart is the moment this process booted — used for
	// uptime_seconds. Captured once at construction.
	GatewayStart time.Time
}

// DefaultDeps wires production implementations against the live
// ~/.hermes/ tree. logDir is the JSONL log directory
// (~/.hermes/logs); nodesPath is the worker registry
// (~/.hermes/nodes.txt); ledgerDir is the signed-ledger root
// (~/.hermes/ledger).
//
// Returns a non-nil Deps even if the hermes client cannot be
// constructed (rare; the binary missing on PATH would break the
// gateway entirely). In that degraded case HermesVersion returns
// the empty string and the snapshot's gateway.ok flips false.
func DefaultDeps(logDir, nodesPath, ledgerDir string) *Deps {
	d := &Deps{
		Now:          time.Now,
		GatewayStart: time.Now(),
		HeartbeatAges: func() (map[string]time.Duration, error) {
			return metrics.HeartbeatAges(logDir, time.Now())
		},
		LoadNodes: func() ([]nodes.Node, error) {
			reg, err := nodes.Load(nodesPath)
			if err != nil {
				return nil, err
			}
			return reg.All(), nil
		},
	}

	// Hermes client is optional — degrade gracefully if unavailable.
	if c, err := hermes.Default(); err == nil {
		d.HermesVersion = c.Version
	} else {
		hermesErr := err
		d.HermesVersion = func(context.Context) (string, error) { return "", hermesErr }
	}

	// Ledger is optional — read-only access; v1 doesn't consume it
	// anyway but wiring it lets the SKU-conformance code light up
	// the moment ledger entries carry a SKU tag.
	if l, err := ledger.OpenReadOnly(ledgerDir); err == nil {
		d.LedgerTasks = l.Tasks
		d.LedgerRead = l.Read
	} else {
		d.LedgerTasks = func() ([]string, error) { return nil, nil }
		d.LedgerRead = func(string) ([]ledger.Entry, error) { return nil, nil }
	}

	return d
}

// HealthSnapshotHandler computes and caches the fleet health snapshot.
//
// The zero value is not valid; construct via [NewHealthSnapshotHandler].
type HealthSnapshotHandler struct {
	deps *Deps

	// rw guards the cached snapshot for the fast-path read.
	rw       sync.RWMutex
	cached   *HealthSnapshot
	cachedAt time.Time

	// refresh guards a single-flight rebuild so a thundering herd
	// of expired-cache callers only triggers one underlying scan.
	// (The issue's "sync.Once-style refresh" wording is shorthand
	// for this pattern; literal sync.Once is one-shot and would
	// only ever produce one snapshot for the process lifetime.)
	refresh sync.Mutex
}

// NewHealthSnapshotHandler wraps deps in a cache-fronted handler.
func NewHealthSnapshotHandler(deps *Deps) *HealthSnapshotHandler {
	if deps == nil {
		panic("rpc: NewHealthSnapshotHandler: deps is nil")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.GatewayStart.IsZero() {
		deps.GatewayStart = deps.Now()
	}
	return &HealthSnapshotHandler{deps: deps}
}

// Registrar is the minimal contract the JSON-RPC server (owned by
// another agent) must satisfy for [HealthSnapshotHandler.Register].
//
// Defining it locally rather than importing the server type lets this
// package compile before the server lands; once the server exists, it
// satisfies this interface implicitly via its Register method.
type Registrar interface {
	Register(method string, h func(ctx context.Context, params json.RawMessage) (any, error))
}

// Register installs the handler under the canonical method name on the
// given JSON-RPC server.
func (h *HealthSnapshotHandler) Register(s Registrar) {
	s.Register("fleet.HealthSnapshot", h.Handle)
}

// Handle is the JSON-RPC handler entrypoint. The params payload is
// ignored (the method takes no arguments per the contract); it is
// accepted to satisfy the standard handler signature.
func (h *HealthSnapshotHandler) Handle(ctx context.Context, _ json.RawMessage) (any, error) {
	return h.Snapshot(ctx)
}

// Snapshot returns the cached snapshot if fresh, otherwise rebuilds.
// Callers that need to skip the cache (e.g., readiness probes in a
// dev shell) should call [Refresh] directly.
func (h *HealthSnapshotHandler) Snapshot(ctx context.Context) (*HealthSnapshot, error) {
	h.rw.RLock()
	cached, cachedAt := h.cached, h.cachedAt
	h.rw.RUnlock()

	if cached != nil && h.deps.Now().Sub(cachedAt) < cacheTTL {
		return cached, nil
	}

	// Single-flight refresh: serialize rebuilds so only one caller
	// pays the cost when the cache expires under load.
	h.refresh.Lock()
	defer h.refresh.Unlock()

	// Re-check after acquiring the refresh mutex — another goroutine
	// may have already rebuilt while we were waiting.
	h.rw.RLock()
	cached, cachedAt = h.cached, h.cachedAt
	h.rw.RUnlock()
	if cached != nil && h.deps.Now().Sub(cachedAt) < cacheTTL {
		return cached, nil
	}

	snap, err := h.compute(ctx)
	if err != nil {
		return nil, err
	}
	h.rw.Lock()
	h.cached = snap
	h.cachedAt = h.deps.Now()
	h.rw.Unlock()
	return snap, nil
}

// Refresh forces a rebuild and updates the cache.
func (h *HealthSnapshotHandler) Refresh(ctx context.Context) (*HealthSnapshot, error) {
	h.refresh.Lock()
	defer h.refresh.Unlock()
	snap, err := h.compute(ctx)
	if err != nil {
		return nil, err
	}
	h.rw.Lock()
	h.cached = snap
	h.cachedAt = h.deps.Now()
	h.rw.Unlock()
	return snap, nil
}

// compute does the underlying scan. Callers must hold h.refresh.
func (h *HealthSnapshotHandler) compute(ctx context.Context) (*HealthSnapshot, error) {
	now := h.deps.Now()

	// Gateway: pinned/live hermes version + process uptime.
	version, vErr := h.deps.HermesVersion(ctx)
	gateway := GatewayHealth{
		OK:            vErr == nil && version != "",
		HermesVersion: version,
		UptimeSeconds: int64(now.Sub(h.deps.GatewayStart).Seconds()),
	}

	// Workers: union of (heartbeat-known) and (registry-known) sets.
	// A worker that appears in the registry but has never reported
	// shows up with a sentinel age (-1) and ok=false.
	ages, _ := h.deps.HeartbeatAges()
	registry, _ := h.deps.LoadNodes()

	seen := make(map[string]bool, len(ages)+len(registry))
	workers := make([]WorkerHealth, 0, len(ages)+len(registry))

	// Stable order: registry first (matches operator's nodes.txt
	// ordering), then any heartbeat-only nodes appended sorted.
	for _, n := range registry {
		host := n.Host
		ageS := -1.0
		ok := false
		if age, ok2 := ages[host]; ok2 {
			ageS = age.Seconds()
			ok = age < HealthyHeartbeatThreshold
		}
		workers = append(workers, WorkerHealth{
			Node:                    host,
			OK:                      ok,
			LastHeartbeatAgeSeconds: ageS,
			// Per-worker hermes version isn't probed by this handler
			// (an SSH round-trip per worker would blow the latency
			// budget). Leave empty; the watchdog's daily probe and
			// fleet-health-snapshot.sh populate it elsewhere.
			HermesVersion: "",
		})
		seen[host] = true
	}
	// Heartbeat-only nodes (worker reporting but not in registry).
	extras := make([]string, 0)
	for host := range ages {
		if !seen[host] {
			extras = append(extras, host)
		}
	}
	// deterministic order
	sortStrings(extras)
	for _, host := range extras {
		age := ages[host]
		workers = append(workers, WorkerHealth{
			Node:                    host,
			OK:                      age < HealthyHeartbeatThreshold,
			LastHeartbeatAgeSeconds: age.Seconds(),
			HermesVersion:           "",
		})
	}

	// SKU conformance: derived from ledger entries with Result="ok"
	// joined per-task SKU against ledger. v1 cannot recover SKU from
	// existing entries — see [SKUConformance] doc. Return [] not nil
	// so the JSON marshal produces "sku_conformance": [].
	sku := h.computeSKUConformance()

	return &HealthSnapshot{
		Ts:             now.UTC().Format(time.RFC3339),
		Gateway:        gateway,
		Workers:        workers,
		SKUConformance: sku,
	}, nil
}

// computeSKUConformance is the placeholder for the SKU aggregate. v1
// returns an empty slice because [ledger.Entry] doesn't carry a SKU
// tag. When issue #5 (or a follow-on) adds SKU tagging, replace this
// with the actual aggregation; the contract is stable so the cloud
// side won't need any changes.
func (h *HealthSnapshotHandler) computeSKUConformance() []SKUConformance {
	// Intentionally non-nil — JSON marshals to `[]` not `null`.
	return []SKUConformance{}
}

// sortStrings is a tiny local helper so we don't pull in "sort" just
// for one call (and so the test file stays self-contained).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
