// snapshot_publish.go implements `fleetctl snapshot-publish` — a
// long-running publisher that polls the local HealthSnapshot handler on
// an interval and writes the result JSON to a target.
//
// The status page at status.owera.ai (WS-19 PR #6) consumes a public
// snapshot object — by convention it sits at
// https://snapshots.owera.ai/health/latest.json. The publisher's job is
// to produce that object on the operator-plane Mac and (optionally)
// upload it. Two delivery modes are supported and may run concurrently:
//
//   1. Atomic file write to --out (always on).
//   2. HTTP PUT to --http-put URL (optional; intended for direct R2/S3
//      upload using a pre-signed token).
package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/log"
	"github.com/owera/owera-fleet/internal/rpc"
)

var (
	snapPublishOut       string
	snapPublishInterval  time.Duration
	snapPublishLogPath   = "~/.hermes/logs/snapshot-publish.jsonl"
	snapPublishOnce      bool
	snapPublishHTTPPut   string
	snapPublishHTTPHdrs  []string
	snapPublishHTTPTimeo time.Duration
)

// Env-var fallback names. Documented in the command's Long help so an
// operator running this from a launchd plist can keep secrets out of
// the command line and process listing.
const (
	envSnapshotHTTPPutURL = "OWERA_SNAPSHOT_HTTP_PUT_URL"
	envSnapshotHTTPAuth   = "OWERA_SNAPSHOT_HTTP_AUTH"
)

var snapshotPublishCmd = &cobra.Command{
	Use:   "snapshot-publish",
	Short: "Periodically write the operator-plane HealthSnapshot JSON to disk (and optionally PUT to R2/S3)",
	Long: `snapshot-publish builds a fleet.HealthSnapshot every --interval and
writes it atomically to --out. The status page (status.owera.ai)
reads the resulting file (either via a public CDN frontend or after
an out-of-band upload step) to render fleet-wide health without
holding a long-lived connection to the operator plane.

Atomic write: the file is written to <out>.tmp first, then renamed
over <out>. Readers never see a half-written JSON.

Defaults:
  --out         ~/.hermes/state/health-snapshot.json
  --interval    30s (must be ≥1s; matches the HealthSnapshot cache TTL)
  --http-timeout 10s

The process runs until SIGINT/SIGTERM. Pass --once to write a single
snapshot and exit (useful for ad-hoc operator commands and for testing
in CI).

HTTP PUT mode (--http-put):
  When --http-put URL is set, after each successful local file write
  the publisher also issues an HTTP PUT with the identical bytes to
  that URL. Intended for direct upload to a Cloudflare R2 or S3
  bucket fronted by a public read-only path. Example:

      fleetctl snapshot-publish \
        --http-put https://snapshots.owera.ai/health/latest.json \
        --http-header "Authorization=Bearer $R2_TOKEN" \
        --http-header "Content-Type=application/json"

  Use --http-header K=V (repeatable) for custom headers (e.g. an R2
  bearer token). Status 2xx is success; anything else is logged and
  counted as a failure. An HTTP PUT failure does NOT halt the loop —
  the local file is the canonical artifact and stale-but-published
  beats no data on the status page.

Env-var fallbacks (so secrets don't end up on the command line for
operators running this from a launchd plist):

  ` + envSnapshotHTTPPutURL + ` — fallback for --http-put when the
      flag is unset.
  ` + envSnapshotHTTPAuth + ` — when --http-header is empty, this is
      sent as a single "Authorization: <value>" header on every PUT.`,
	Example: `  fleetctl snapshot-publish
  fleetctl snapshot-publish --out /var/owera/health.json --interval 15s
  fleetctl snapshot-publish --once  # one snapshot, exit non-zero on failure
  fleetctl snapshot-publish \
      --http-put https://snapshots.owera.ai/health/latest.json \
      --http-header "Authorization=Bearer $R2_TOKEN"`,
	RunE: runSnapshotPublish,
}

func init() {
	snapshotPublishCmd.Flags().StringVar(&snapPublishOut, "out", "~/.hermes/state/health-snapshot.json", "destination file path (atomic write via .tmp + rename)")
	snapshotPublishCmd.Flags().DurationVar(&snapPublishInterval, "interval", 30*time.Second, "poll interval (≥1s)")
	snapshotPublishCmd.Flags().BoolVar(&snapPublishOnce, "once", false, "build one snapshot, write it, and exit")
	snapshotPublishCmd.Flags().StringVar(&snapPublishHTTPPut, "http-put", "", "HTTP URL to PUT the snapshot JSON to after each write (env fallback: "+envSnapshotHTTPPutURL+")")
	snapshotPublishCmd.Flags().StringArrayVar(&snapPublishHTTPHdrs, "http-header", nil, "repeatable HTTP header K=V to include on the PUT (env fallback: "+envSnapshotHTTPAuth+" → Authorization)")
	snapshotPublishCmd.Flags().DurationVar(&snapPublishHTTPTimeo, "http-timeout", 10*time.Second, "HTTP PUT request timeout")
	rootCmd.AddCommand(snapshotPublishCmd)
	registerSkill("snapshot-publish", snapshotPublishSkill)
}

func runSnapshotPublish(cmd *cobra.Command, _ []string) error {
	if snapPublishInterval < time.Second {
		return fmt.Errorf("snapshot-publish: interval %s < 1s", snapPublishInterval)
	}
	outPath, err := expandHomePath(snapPublishOut)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("snapshot-publish: mkdir %s: %w", filepath.Dir(outPath), err)
	}

	hermesHome, err := expandHomePath("~/.hermes")
	if err != nil {
		return err
	}
	resolvedLog, err := expandHomePath(snapPublishLogPath)
	if err != nil {
		return err
	}
	logger, err := openLogger(resolvedLog)
	if err != nil {
		return fmt.Errorf("snapshot-publish: open log: %w", err)
	}
	defer logger.Close()

	// Cache TTL on the handler is 30s; passing through the cache means
	// concurrent /readyz probes against fleet.HealthSnapshot share the
	// same compute. Building a fresh handler per process (rather than
	// reusing serve's) keeps the publisher independent of whether
	// `fleetctl serve` is also running.
	deps := rpc.DefaultDeps(
		hermesHome+"/logs",
		hermesHome+"/nodes.txt",
		hermesHome+"/ledger",
	)
	handler := rpc.NewHealthSnapshotHandler(deps)

	httpCfg, err := resolveHTTPPutConfig(snapPublishHTTPPut, snapPublishHTTPHdrs, snapPublishHTTPTimeo)
	if err != nil {
		return fmt.Errorf("snapshot-publish: %w", err)
	}

	if snapPublishOnce {
		return publishOnce(cmd.Context(), handler, outPath, logger, httpCfg)
	}

	return publishLoop(cmd, handler, outPath, logger, snapPublishInterval, httpCfg)
}

// httpPutConfig captures the resolved HTTP PUT target after merging
// flags and env fallbacks. URL == "" means HTTP PUT is disabled.
type httpPutConfig struct {
	URL     string
	Headers map[string]string
	Timeout time.Duration
}

// Enabled reports whether the publisher should issue a PUT each cycle.
func (h httpPutConfig) Enabled() bool { return h.URL != "" }

// resolveHTTPPutConfig merges --http-put / --http-header / --http-timeout
// with the documented env-var fallbacks. K=V headers are parsed once
// here so the per-cycle path is allocation-light. The fallback rule:
//   - if --http-put is empty, use $OWERA_SNAPSHOT_HTTP_PUT_URL
//   - if --http-header is empty AND $OWERA_SNAPSHOT_HTTP_AUTH is set,
//     send it as the single "Authorization" header.
//
// Headers from --http-header always win — env auth is only a fallback
// when the flag is entirely unset, so operators can override on the
// CLI even when the plist sets the env.
func resolveHTTPPutConfig(url string, headerFlags []string, timeout time.Duration) (httpPutConfig, error) {
	cfg := httpPutConfig{Timeout: timeout}
	if url == "" {
		url = os.Getenv(envSnapshotHTTPPutURL)
	}
	cfg.URL = url
	if cfg.URL == "" {
		return cfg, nil
	}
	if timeout <= 0 {
		return cfg, fmt.Errorf("http-timeout %s must be > 0", timeout)
	}

	hdrs := make(map[string]string, len(headerFlags)+1)
	if len(headerFlags) == 0 {
		if auth := os.Getenv(envSnapshotHTTPAuth); auth != "" {
			hdrs["Authorization"] = auth
		}
	} else {
		for _, raw := range headerFlags {
			k, v, ok := strings.Cut(raw, "=")
			if !ok {
				return cfg, fmt.Errorf("http-header %q: expected K=V", raw)
			}
			k = strings.TrimSpace(k)
			v = strings.TrimSpace(v)
			if k == "" {
				return cfg, fmt.Errorf("http-header %q: empty key", raw)
			}
			hdrs[k] = v
		}
	}
	cfg.Headers = hdrs
	return cfg, nil
}

// publishOnce builds a single snapshot and writes it atomically. If
// the HTTP PUT target is configured, it is also issued after a
// successful file write; PUT failures are logged but do NOT cause the
// command to exit non-zero (the local file is the canonical artifact).
func publishOnce(ctx context.Context, handler *rpc.HealthSnapshotHandler, outPath string, logger *log.Logger, httpCfg httpPutConfig) error {
	snap, err := handler.Snapshot(ctx)
	if err != nil {
		_ = logger.Action(log.Event{
			Node: "gateway", Phase: "snapshot-publish",
			Action: "build", Result: log.ResultError, StderrTail: err.Error(),
		})
		return fmt.Errorf("snapshot-publish: build: %w", err)
	}
	body, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		_ = logger.Action(log.Event{
			Node: "gateway", Phase: "snapshot-publish",
			Action: "marshal", Result: log.ResultError, StderrTail: err.Error(),
		})
		return fmt.Errorf("snapshot-publish: marshal: %w", err)
	}
	if err := writeBytesAtomic(outPath, body); err != nil {
		_ = logger.Action(log.Event{
			Node: "gateway", Phase: "snapshot-publish",
			Action: "write:" + outPath, Result: log.ResultError, StderrTail: err.Error(),
		})
		return fmt.Errorf("snapshot-publish: write: %w", err)
	}
	_ = logger.Action(log.Event{
		Node: "gateway", Phase: "snapshot-publish",
		Action: "write:" + outPath, Result: log.ResultOK,
	})
	if httpCfg.Enabled() {
		doHTTPPut(ctx, httpCfg, body, logger)
	}
	return nil
}

// publishLoop runs the build → write → optional PUT cycle on a ticker
// until SIGINT/SIGTERM. An individual cycle failure is logged but does
// not stop the loop — the status page is best-served by stale-but-
// present data over no data.
func publishLoop(cmd *cobra.Command, handler *rpc.HealthSnapshotHandler, outPath string, logger *log.Logger, interval time.Duration, httpCfg httpPutConfig) error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Number of consecutive cycle failures — for monitoring purposes only.
	var consecutiveFailures atomic.Int64
	// HTTP PUT failure counter — independent of file-write failures.
	// Surfaced in the JSONL log; status-page operators can scrape it.
	var httpPutFailures atomic.Int64

	publish := func() {
		ctx, cancel := context.WithTimeout(cmd.Context(), interval/2)
		defer cancel()
		snap, err := handler.Snapshot(ctx)
		if err != nil {
			consecutiveFailures.Add(1)
			_ = logger.Action(log.Event{
				Node: "gateway", Phase: "snapshot-publish",
				Action: "build", Result: log.ResultError, StderrTail: err.Error(),
			})
			return
		}
		body, err := json.MarshalIndent(snap, "", "  ")
		if err != nil {
			consecutiveFailures.Add(1)
			_ = logger.Action(log.Event{
				Node: "gateway", Phase: "snapshot-publish",
				Action: "marshal", Result: log.ResultError, StderrTail: err.Error(),
			})
			return
		}
		if err := writeBytesAtomic(outPath, body); err != nil {
			consecutiveFailures.Add(1)
			_ = logger.Action(log.Event{
				Node: "gateway", Phase: "snapshot-publish",
				Action: "write:" + outPath, Result: log.ResultError, StderrTail: err.Error(),
			})
			return
		}
		consecutiveFailures.Store(0)
		_ = logger.Action(log.Event{
			Node: "gateway", Phase: "snapshot-publish",
			Action: "write:" + outPath, Result: log.ResultOK,
		})
		if httpCfg.Enabled() {
			if ok := doHTTPPut(ctx, httpCfg, body, logger); !ok {
				httpPutFailures.Add(1)
			}
		}
	}

	// First publish runs immediately, not at first tick.
	publish()
	if httpCfg.Enabled() {
		fmt.Fprintf(cmd.OutOrStdout(), "fleetctl snapshot-publish: writing %s every %s; PUT → %s\n", outPath, interval, httpCfg.URL)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "fleetctl snapshot-publish: writing %s every %s\n", outPath, interval)
	}

	for {
		select {
		case <-sigCh:
			fmt.Fprintln(cmd.OutOrStdout(), "fleetctl snapshot-publish: shutting down")
			return nil
		case <-ticker.C:
			publish()
		}
	}
}

// doHTTPPut issues a single PUT to the configured URL with the exact
// bytes that landed on disk. 2xx is success. Anything else (network
// error, timeout, non-2xx status) is logged and reported as a failure.
// Crucially this does NOT propagate the error to the caller — the
// loop must keep running. Returns true on success.
func doHTTPPut(ctx context.Context, cfg httpPutConfig, body []byte, logger *log.Logger) bool {
	if err := putJSON(ctx, cfg.URL, body, cfg.Headers, cfg.Timeout); err != nil {
		_ = logger.Action(log.Event{
			Node: "gateway", Phase: "snapshot-publish",
			Action: "http-put:" + cfg.URL, Result: log.ResultError, StderrTail: err.Error(),
		})
		return false
	}
	_ = logger.Action(log.Event{
		Node: "gateway", Phase: "snapshot-publish",
		Action: "http-put:" + cfg.URL, Result: log.ResultOK,
	})
	return true
}

// putJSON sends body as the PUT body to url with the supplied headers
// and request timeout. Non-2xx responses become an error so the
// caller can log + count them as a failure.
func putJSON(ctx context.Context, url string, body []byte, headers map[string]string, timeout time.Duration) error {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	hasContentType := false
	for k, v := range headers {
		req.Header.Set(k, v)
		if strings.EqualFold(k, "Content-Type") {
			hasContentType = true
		}
	}
	if !hasContentType {
		req.Header.Set("Content-Type", "application/json")
	}
	req.ContentLength = int64(len(body))

	client := snapshotHTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("PUT %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("PUT %s: status %d", url, resp.StatusCode)
	}
	return nil
}

// snapshotHTTPClient is overridable for tests that want to use the
// httptest server's Client() (e.g. for TLS). When nil a fresh
// http.Client is constructed per call with the per-cycle timeout.
var snapshotHTTPClient *http.Client

// writeBytesAtomic writes data to outPath via a sibling .tmp file and
// renames it over the target. Readers always observe a complete file
// or the previous version — never half-written bytes.
func writeBytesAtomic(outPath string, data []byte) error {
	tmp := outPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	// fsync the tmp file before rename so the rename can't promote a
	// partially-written file to the canonical path on a crash.
	f, err := os.Open(tmp)
	if err != nil {
		return fmt.Errorf("reopen tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmp, outPath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// writeJSONAtomic is kept for backward compatibility with existing
// tests that exercise the marshal+write path as one unit.
func writeJSONAtomic(outPath string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return writeBytesAtomic(outPath, data)
}

// snapshotPublishSkill is the skill manifest entry consumed by
// `fleetctl gen-skills`.
func snapshotPublishSkill() Skill {
	return Skill{
		Name:    "snapshot-publish",
		Trigger: "/snapshot-publish",
		Summary: "Periodically write the operator-plane HealthSnapshot JSON to disk (and optionally PUT to R2/S3) for the status page to consume.",
		Args: []SkillArg{
			{Name: "--out PATH", Description: "Destination file (atomic via .tmp + rename). Default ~/.hermes/state/health-snapshot.json."},
			{Name: "--interval DUR", Description: "Poll interval (≥1s; matches the HealthSnapshot cache TTL). Default 30s."},
			{Name: "--once", Description: "Write a single snapshot and exit (CI / ad-hoc shape)."},
			{Name: "--http-put URL", Description: "Also PUT the JSON body to this URL each cycle (env fallback: " + envSnapshotHTTPPutURL + "). Failures are logged but don't halt the loop."},
			{Name: "--http-header K=V", Description: "Repeatable header for --http-put requests (env fallback: " + envSnapshotHTTPAuth + " → Authorization)."},
			{Name: "--http-timeout DUR", Description: "Per-request timeout for --http-put. Default 10s."},
		},
	}
}
