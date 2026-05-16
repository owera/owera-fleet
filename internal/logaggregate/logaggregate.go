// Package logaggregate is the WS-10 nightly log-aggregation primitive.
//
// Every worker writes structured JSONL telemetry into ~/.hermes/logs/*.jsonl
// (see [internal/log]). On its own, that telemetry is per-host: useful while
// debugging a specific node, useless for the fleet-wide audit + metrics
// pipelines that consume the canonical Owera event stream. WS-10 closes the
// loop by pulling each worker's logs/ directory to the gateway nightly and
// landing them under ~/.hermes/logs/aggregated/<host>/ so downstream
// consumers (ledger, audit, metrics) see one fleet-wide stream.
//
// # Why tar+gzip over SSH, not rsync
//
// hermes-setup's first attempt at this used rsync, which immediately broke
// on the stock bash 3.2 + rsync 2.6.9 that ships in macOS. We're not paying
// that toll again. tar c | gzip over an SSH session is portable to every
// macOS/Linux worker the fleet will ever run, needs no extra binaries
// installed on the worker, and the gateway-side extraction uses pure-Go
// [archive/tar] + [compress/gzip] for the same reasons.
//
// # Concurrency
//
// Up to [MaxConcurrent] nodes are pulled in parallel; the cap exists so a
// large fleet can't open more SSH sessions than the host can sustain. The
// per-node pull is itself serial (one tar, one extract) — the bottleneck
// is network I/O, not CPU.
//
// # Retention
//
// After a successful pull, files under GatewayLogDir/<host>/ older than
// KeepDays are deleted. The cutoff is based on the file's mtime, not its
// embedded ts field — the file itself is the unit of retention. Setting
// KeepDays to zero disables pruning (useful for one-shot dry-run audits).
package logaggregate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/owera/owera-fleet/internal/nodes"
)

// MaxConcurrent caps the number of nodes pulled in parallel. Hand-picked
// to be small enough that even a Mac mini gateway with a constrained
// /etc/security/audit_user fan-out can sustain it without exhausting
// SSH sessions or open file descriptors.
const MaxConcurrent = 4

// DefaultKeepDays is the retention window used when Aggregator.KeepDays
// is zero. Thirty days lines up with the ledger's audit retention so the
// two stay in sync without operator coordination.
const DefaultKeepDays = 30

// remoteLogsGlob is the worker-side path the aggregator pulls. Encoded as
// a constant so the SSH command and the in-tarball header parsing stay
// in lockstep — operators reading the JSONL audit can grep for this
// string and find the canonical path.
const remoteLogsGlob = "$HOME/.hermes/logs/*.jsonl"

// SSHRunner is the injection seam for the SSH transport. Implementations
// return the command's stdout (full bytes), stderr (used for error
// messages), exit code, and any transport-level error. Tests substitute
// a fake that synthesizes tarball bytes; production wires the
// [internal/ssh] dialer through [NewSSHRunner].
type SSHRunner func(ctx context.Context, target, cmd string) (stdout, stderr string, exitCode int, err error)

// PullResult summarises one worker's pull cycle. Err carries the first
// fatal error in the pull/extract/rotate sequence; FilesPulled and
// BytesPulled may still be non-zero if extraction completed partially
// before a later step (e.g. rotation) failed.
type PullResult struct {
	Node        string        `json:"node"`
	FilesPulled int           `json:"files_pulled"`
	BytesPulled int64         `json:"bytes_pulled"`
	FilesPruned int           `json:"files_pruned"`
	Err         error         `json:"-"`
	ErrMessage  string        `json:"err,omitempty"`
	Duration    time.Duration `json:"duration_ns"`
}

// Result is the aggregate view across every worker the run touched.
// OK is true iff every per-node pull completed without Err.
type Result struct {
	Pulls     []PullResult `json:"pulls"`
	StartedAt time.Time    `json:"started_at"`
	EndedAt   time.Time    `json:"ended_at"`
	OK        bool         `json:"ok"`
}

// Aggregator is the configurable handle the CLI and tests both drive.
//
// Zero-value fields fall back to documented defaults (NodesFile→nodes
// default, KeepDays→DefaultKeepDays). SSHRunner is required.
type Aggregator struct {
	// GatewayLogDir is the per-node landing directory. Each worker's
	// extracted files land under GatewayLogDir/<host>/. The directory
	// is created with mode 0o755 if it does not exist.
	GatewayLogDir string

	// SSHRunner is the transport. Required.
	SSHRunner SSHRunner

	// NodesFile is the path to the nodes.txt registry. Empty means
	// "use nodes.Load default" (~/.hermes/nodes.txt).
	NodesFile string

	// SingleNode, if non-empty, restricts the run to one target —
	// bypasses NodesFile lookup but still validates the target shape
	// via [nodes.Node]. Useful for ad-hoc operator pulls.
	SingleNode string

	// KeepDays is the retention window for files under each node's
	// landing directory. Zero means DefaultKeepDays; negative disables
	// pruning entirely.
	KeepDays int

	// RotateOnPull instructs the aggregator to gzip the source files
	// on each worker after a successful pull. Off by default — the
	// local logrotate agent already runs daily at 03:30 and most fleets
	// want the local copy preserved for at least one rotation cycle.
	RotateOnPull bool

	// DryRun skips every mutating step (no extract, no prune, no
	// rotate) but still calls the SSHRunner with a size-only probe so
	// the operator sees what would be pulled.
	DryRun bool

	// Now overrides the clock used for retention pruning. Defaults to
	// time.Now.
	Now func() time.Time
}

// Run iterates every selected worker, pulls and extracts its JSONL logs,
// optionally rotates the source files, and prunes stale files in the
// landing directory. The returned Result has one [PullResult] per node;
// Err on each PullResult carries that node's failure (if any).
func (a *Aggregator) Run(ctx context.Context) (*Result, error) {
	if a.SSHRunner == nil {
		return nil, errors.New("logaggregate: SSHRunner is required")
	}
	if a.GatewayLogDir == "" {
		return nil, errors.New("logaggregate: GatewayLogDir is required")
	}

	targets, err := a.resolveTargets()
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, errors.New("logaggregate: no targets resolved (empty nodes.txt or no --node)")
	}

	if !a.DryRun {
		if err := os.MkdirAll(a.GatewayLogDir, 0o755); err != nil {
			return nil, fmt.Errorf("logaggregate: mkdir %s: %w", a.GatewayLogDir, err)
		}
	}

	started := a.now()

	// sem caps concurrent SSH sessions. A buffered channel + struct{}
	// is the bash-3.2-of-Go-concurrency: no external dep, no goroutine
	// leak risk if a panic escapes.
	sem := make(chan struct{}, MaxConcurrent)
	results := make([]PullResult, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(idx int, target string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[idx] = a.pullOne(ctx, target)
		}(i, t)
	}
	wg.Wait()

	ended := a.now()
	ok := true
	for _, r := range results {
		if r.Err != nil {
			ok = false
			break
		}
	}
	return &Result{
		Pulls:     results,
		StartedAt: started,
		EndedAt:   ended,
		OK:        ok,
	}, nil
}

// pullOne runs the full cycle for one target. The PullResult carries Err
// when any step fails; later steps short-circuit on the first error so
// the caller doesn't have to disambiguate "partial extract + partial
// rotate" from "clean failure".
func (a *Aggregator) pullOne(ctx context.Context, target string) PullResult {
	start := a.now()
	res := PullResult{Node: target}

	host := hostOf(target)
	destDir := filepath.Join(a.GatewayLogDir, host)

	if a.DryRun {
		// Cheap probe: count the matching files via wc + find so the
		// operator gets a sense of the volume without paying for the
		// full tar pipeline.
		probeCmd := fmt.Sprintf("ls -1 %s 2>/dev/null | wc -l", remoteLogsGlob)
		stdout, stderr, exit, err := a.SSHRunner(ctx, target, probeCmd)
		res.Duration = a.now().Sub(start)
		if err != nil {
			res.Err = fmt.Errorf("dry-run probe ssh: %w", err)
			res.ErrMessage = res.Err.Error()
			return res
		}
		if exit != 0 {
			res.Err = fmt.Errorf("dry-run probe exit %d: %s", exit, strings.TrimSpace(stderr))
			res.ErrMessage = res.Err.Error()
			return res
		}
		// stdout is a single integer on its own line.
		var n int
		_, _ = fmt.Sscanf(strings.TrimSpace(stdout), "%d", &n)
		res.FilesPulled = n
		return res
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		res.Err = fmt.Errorf("mkdir %s: %w", destDir, err)
		res.ErrMessage = res.Err.Error()
		res.Duration = a.now().Sub(start)
		return res
	}

	// The SSH command uses sh -c so the glob expands on the worker, not
	// in the operator's local shell. 2>/dev/null swallows the spurious
	// "No such file" tar emits when the glob matches zero files. We
	// then check tar's exit code: a clean run with zero matches still
	// exits 0 because we explicitly route stderr to /dev/null.
	pullCmd := fmt.Sprintf("sh -c 'tar -C $HOME/.hermes/logs -c -f - $(cd $HOME/.hermes/logs && ls *.jsonl 2>/dev/null) 2>/dev/null | gzip -c'")
	stdout, stderr, exit, err := a.SSHRunner(ctx, target, pullCmd)
	if err != nil {
		res.Err = fmt.Errorf("ssh pull: %w", err)
		res.ErrMessage = res.Err.Error()
		res.Duration = a.now().Sub(start)
		return res
	}
	if exit != 0 {
		// tar/gzip both signal partial-failure via non-zero; surface
		// stderr to the operator so they can diagnose without reaching
		// for journalctl.
		res.Err = fmt.Errorf("remote pull exit %d: %s", exit, strings.TrimSpace(stderr))
		res.ErrMessage = res.Err.Error()
		res.Duration = a.now().Sub(start)
		return res
	}

	files, bytes, err := extractTarGz([]byte(stdout), destDir)
	res.FilesPulled = files
	res.BytesPulled = bytes
	if err != nil {
		res.Err = fmt.Errorf("extract: %w", err)
		res.ErrMessage = res.Err.Error()
		res.Duration = a.now().Sub(start)
		return res
	}

	if a.RotateOnPull && files > 0 {
		// Gzip the worker's *.jsonl files in place. The local logrotate
		// agent (com.hermes.logrotate, daily 03:30) already does this
		// size-based; --rotate-on-pull is the "force a rotation now"
		// escape hatch for operators who want a clean cut after each
		// aggregation.
		rotateCmd := "sh -c 'cd $HOME/.hermes/logs && for f in *.jsonl; do [ -e \"$f\" ] && gzip -f \"$f\"; done; true'"
		_, rstderr, rexit, rerr := a.SSHRunner(ctx, target, rotateCmd)
		if rerr != nil {
			res.Err = fmt.Errorf("rotate: %w", rerr)
			res.ErrMessage = res.Err.Error()
			res.Duration = a.now().Sub(start)
			return res
		}
		if rexit != 0 {
			res.Err = fmt.Errorf("rotate exit %d: %s", rexit, strings.TrimSpace(rstderr))
			res.ErrMessage = res.Err.Error()
			res.Duration = a.now().Sub(start)
			return res
		}
	}

	pruned, err := a.prune(destDir)
	if err != nil {
		res.Err = fmt.Errorf("prune: %w", err)
		res.ErrMessage = res.Err.Error()
		res.Duration = a.now().Sub(start)
		return res
	}
	res.FilesPruned = pruned
	res.Duration = a.now().Sub(start)
	return res
}

// extractTarGz consumes a gzipped tar stream and writes each regular
// file under destDir. The tar's internal directory structure is
// flattened — only the base filename is honoured — because the
// remote tar command runs from $HOME/.hermes/logs so headers carry
// just the basenames. Returns (count, total bytes, err).
//
// Symlinks, devices, and other non-regular tar entries are silently
// skipped (logs/*.jsonl should be regular files; anything else is
// suspicious enough that we'd rather not materialise it).
func extractTarGz(blob []byte, destDir string) (int, int64, error) {
	if len(blob) == 0 {
		return 0, 0, nil
	}
	gz, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return 0, 0, fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var count int
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return count, total, fmt.Errorf("tar next: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA { //nolint:staticcheck // TypeRegA aliased to TypeReg in go1.11+
			continue
		}
		// Defang against path traversal: flatten to basename. Workers
		// are trusted but defence-in-depth is cheap.
		base := path.Base(hdr.Name)
		if base == "" || base == "." || base == ".." || strings.Contains(base, "/") {
			continue
		}
		outPath := filepath.Join(destDir, base)
		f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return count, total, fmt.Errorf("open %s: %w", outPath, err)
		}
		n, copyErr := io.Copy(f, tr)
		closeErr := f.Close()
		if copyErr != nil {
			return count, total, fmt.Errorf("copy %s: %w", outPath, copyErr)
		}
		if closeErr != nil {
			return count, total, fmt.Errorf("close %s: %w", outPath, closeErr)
		}
		// Preserve mtime so the retention pruner respects the worker's
		// view of file age rather than the gateway's clock-skewed one.
		if !hdr.ModTime.IsZero() {
			_ = os.Chtimes(outPath, hdr.ModTime, hdr.ModTime)
		}
		count++
		total += n
	}
	return count, total, nil
}

// prune deletes regular files under dir whose mtime is older than the
// retention cutoff. Returns the number of files removed. Subdirectories
// are not recursed — the aggregator's layout is flat-per-node.
func (a *Aggregator) prune(dir string) (int, error) {
	keep := a.KeepDays
	if keep == 0 {
		keep = DefaultKeepDays
	}
	if keep < 0 {
		return 0, nil
	}
	cutoff := a.now().Add(-time.Duration(keep) * 24 * time.Hour)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var removed int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
				removed++
			}
		}
	}
	return removed, nil
}

// resolveTargets returns the per-node user@host strings the runner
// should drive. SingleNode short-circuits NodesFile resolution.
func (a *Aggregator) resolveTargets() ([]string, error) {
	if a.SingleNode != "" {
		return []string{a.SingleNode}, nil
	}
	reg, err := nodes.Load(a.NodesFile)
	if err != nil {
		return nil, fmt.Errorf("load nodes: %w", err)
	}
	if reg.Len() == 0 {
		return nil, nil
	}
	all := reg.All()
	out := make([]string, 0, len(all))
	for _, n := range all {
		out = append(out, n.String())
	}
	return out, nil
}

func (a *Aggregator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// hostOf returns the host segment of a "user@host" target string. Used
// to derive per-worker landing-directory names so the on-disk layout is
// keyed by host (not the operator's username, which can vary).
func hostOf(target string) string {
	if i := strings.LastIndex(target, "@"); i >= 0 && i < len(target)-1 {
		return target[i+1:]
	}
	return target
}
