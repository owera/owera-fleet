// snapshot_publish.go implements `fleetctl snapshot-publish` — a
// long-running publisher that polls the local HealthSnapshot handler on
// an interval and writes the result JSON to a target.
//
// The status page at status.owera.ai (WS-19 PR #6) consumes a public
// snapshot object — by convention it sits at
// https://snapshots.owera.ai/health/latest.json. The publisher's job is
// to produce that object on the operator-plane Mac and (in a later
// follow-up) upload it. For V0 the publisher writes a local file
// atomically; operators wire that file to a separate uploader (cron
// rsync, R2 PUT script, etc.) until first-class R2/S3 PUT lands here.
package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
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
)

var snapshotPublishCmd = &cobra.Command{
	Use:   "snapshot-publish",
	Short: "Periodically write the operator-plane HealthSnapshot JSON to disk",
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

The process runs until SIGINT/SIGTERM. Pass --once to write a single
snapshot and exit (useful for ad-hoc operator commands and for testing
in CI).`,
	Example: `  fleetctl snapshot-publish
  fleetctl snapshot-publish --out /var/owera/health.json --interval 15s
  fleetctl snapshot-publish --once  # one snapshot, exit non-zero on failure`,
	RunE: runSnapshotPublish,
}

func init() {
	snapshotPublishCmd.Flags().StringVar(&snapPublishOut, "out", "~/.hermes/state/health-snapshot.json", "destination file path (atomic write via .tmp + rename)")
	snapshotPublishCmd.Flags().DurationVar(&snapPublishInterval, "interval", 30*time.Second, "poll interval (≥1s)")
	snapshotPublishCmd.Flags().BoolVar(&snapPublishOnce, "once", false, "build one snapshot, write it, and exit")
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

	if snapPublishOnce {
		return publishOnce(cmd.Context(), handler, outPath, logger)
	}

	return publishLoop(cmd, handler, outPath, logger, snapPublishInterval)
}

// publishOnce builds a single snapshot and writes it atomically. Errors
// surface as the command's exit code.
func publishOnce(ctx context.Context, handler *rpc.HealthSnapshotHandler, outPath string, logger *log.Logger) error {
	snap, err := handler.Snapshot(ctx)
	if err != nil {
		_ = logger.Action(log.Event{
			Node: "gateway", Phase: "snapshot-publish",
			Action: "build", Result: log.ResultError, StderrTail: err.Error(),
		})
		return fmt.Errorf("snapshot-publish: build: %w", err)
	}
	if err := writeJSONAtomic(outPath, snap); err != nil {
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
	return nil
}

// publishLoop runs publishOnce on a ticker until SIGINT/SIGTERM. An
// individual cycle failure is logged but does not stop the loop — the
// status page is best-served by stale-but-present data over no data.
func publishLoop(cmd *cobra.Command, handler *rpc.HealthSnapshotHandler, outPath string, logger *log.Logger, interval time.Duration) error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Number of consecutive cycle failures — for monitoring purposes only.
	var consecutiveFailures atomic.Int64

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
		if err := writeJSONAtomic(outPath, snap); err != nil {
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
	}

	// First publish runs immediately, not at first tick.
	publish()
	fmt.Fprintf(cmd.OutOrStdout(), "fleetctl snapshot-publish: writing %s every %s\n", outPath, interval)

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

// writeJSONAtomic serialises v to outPath via a sibling .tmp file and
// renames it over the target. Readers always observe a complete file
// or the previous version — never half-written bytes.
func writeJSONAtomic(outPath string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
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

// snapshotPublishSkill is the skill manifest entry consumed by
// `fleetctl gen-skills`.
func snapshotPublishSkill() Skill {
	return Skill{
		Name:    "snapshot-publish",
		Trigger: "/snapshot-publish",
		Summary: "Periodically write the operator-plane HealthSnapshot JSON to disk for the status page to consume.",
		Args: []SkillArg{
			{Name: "--out PATH", Description: "Destination file (atomic via .tmp + rename). Default ~/.hermes/state/health-snapshot.json."},
			{Name: "--interval DUR", Description: "Poll interval (≥1s; matches the HealthSnapshot cache TTL). Default 30s."},
			{Name: "--once", Description: "Write a single snapshot and exit (CI / ad-hoc shape)."},
		},
	}
}

// errIntervalTooShort is kept package-level so tests don't need to
// duplicate the message string.
var errIntervalTooShort = errors.New("snapshot-publish: interval < 1s")
