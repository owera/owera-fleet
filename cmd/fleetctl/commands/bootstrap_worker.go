// bootstrap_worker.go implements `fleetctl bootstrap-worker` — the typed
// Go replacement for scripts/bootstrap-hermes-node.sh.
//
// The command iterates the bootstrap pipeline's ten phase scripts under
// remote/phaseNN_*.sh (phase00 through phase09), in order:
//
//   00  brew_baseline          ensure Homebrew + the essentials list
//   01  verify_prereqs         sudo / brew / xcode-clt all reachable
//   02  create_user            unprivileged 'hermes' user
//   03  install_hermes         install hermes at the pinned version
//   04  seed_config            drop config.yaml + tirith_path rewrite
//   05  distribute_secrets     .env.gpg + run-with-secrets.sh
//   06  setup_dedicated_key    install gateway pubkey in authorized_keys
//   07  install_tirith         best-effort tirith install
//   08  launchd_heartbeat      heartbeat LaunchDaemon
//   09  verify                 acceptance gate
//
// Each phase is uploaded with scp-like semantics, executed via SSH, and
// its stderr is parsed for JSONL telemetry. Stops on first failed phase.
package commands

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/bootstrap"
	"github.com/owera/owera-fleet/internal/log"
	osh "github.com/owera/owera-fleet/internal/ssh"
)

var (
	bwNode             string
	bwPhase            string
	bwScriptDir        string
	bwDryRun           bool
	bwQuiet            bool
	bwTimeout          time.Duration
	bwLogPath          = "~/.hermes/logs/bootstrap.jsonl"
	bwPinnedVersion    string
	bwPinnedFile       string
	bwConfigBundleSrc  string
	bwEnvGPG           string
	bwSecretsWrapper   string
	bwDedicatedKeyPub  string
	bwPlistTemplate    string
	bwGatewayPrefix    string
	bwAllowExistAdmin  bool
	bwSkipTirith       bool
	bwSkipHeartbeat    bool
	bwHeartbeatWaitSec int
)

const (
	bwDefaultPinnedFile      = "~/.hermes/PINNED_VERSION"
	bwDefaultEnvGPG          = "~/.hermes/.env.gpg"
	bwDefaultDedicatedKey    = "~/.hermes_ssh_key"
	bwDefaultGatewayPrefix   = "~/.hermes"
	bwDefaultPlistTemplate   = "templates/com.hermes.heartbeat.plist"
	bwDefaultHeartbeatWait   = 70
	bwDefaultHermesUser      = "hermes"
)

// phaseList is the canonical ordered list of phase scripts.
var phaseList = []string{
	"phase00_brew_baseline.sh",
	"phase01_verify_prereqs.sh",
	"phase02_create_user.sh",
	"phase03_install_hermes.sh",
	"phase04_seed_config.sh",
	"phase05_distribute_secrets.sh",
	"phase06_setup_dedicated_key.sh",
	"phase07_install_tirith.sh",
	"phase08_launchd_heartbeat.sh",
	"phase09_verify.sh",
}

var bootstrapWorkerCmd = &cobra.Command{
	Use:   "bootstrap-worker",
	Short: "Provision a worker by running the multi-phase bootstrap pipeline",
	Long: `bootstrap-worker runs the ten remote bootstrap phase scripts under
remote/phaseNN_*.sh on a worker node via SSH, capturing per-action JSONL
telemetry.

Phases (in execution order):
  00 brew_baseline       Homebrew + essentials baseline
  01 verify_prereqs      sudo / brew / xcode-clt reachable
  02 create_user         unprivileged 'hermes' user
  03 install_hermes      install hermes at the pinned version
  04 seed_config         drop config.yaml + rewrite tirith_path
  05 distribute_secrets  .env.gpg + run-with-secrets.sh
  06 setup_dedicated_key install gateway pubkey in authorized_keys
  07 install_tirith      best-effort tirith install
  08 launchd_heartbeat   heartbeat LaunchDaemon
  09 verify              acceptance gate (final JSONL 'verify' event)

Use --phase to run a single named phase.
Use --dry-run to run scripts with --dry-run flag (probe-only, no mutations).

All phase events are appended to ~/.hermes/logs/bootstrap.jsonl.`,
	Example: `  fleetctl bootstrap-worker --node hermes@claw1.local
  fleetctl bootstrap-worker --node hermes@claw1.local --phase phase00_brew_baseline.sh
  fleetctl bootstrap-worker --node hermes@claw1.local --dry-run`,
	RunE: runBootstrapWorker,
}

func init() {
	bootstrapWorkerCmd.Flags().StringVar(&bwNode, "node", "", "target worker user@host (required)")
	bootstrapWorkerCmd.Flags().StringVar(&bwPhase, "phase", "", "run a single named phase script (default: all 10 phases in order)")
	bootstrapWorkerCmd.Flags().StringVar(&bwScriptDir, "script-dir", "", "override local remote/ directory")
	bootstrapWorkerCmd.Flags().BoolVar(&bwDryRun, "dry-run", false, "pass --dry-run to phase scripts (probe only)")
	bootstrapWorkerCmd.Flags().BoolVar(&bwQuiet, "quiet", false, "suppress stdout; JSONL log still written")
	bootstrapWorkerCmd.Flags().DurationVar(&bwTimeout, "timeout", 10*time.Minute, "per-phase timeout")

	bootstrapWorkerCmd.Flags().StringVar(&bwPinnedVersion, "pinned-version", "",
		"override pinned Hermes version (default: read from --pinned-version-file)")
	bootstrapWorkerCmd.Flags().StringVar(&bwPinnedFile, "pinned-version-file", bwDefaultPinnedFile,
		"gateway-side PINNED_VERSION file")
	bootstrapWorkerCmd.Flags().StringVar(&bwConfigBundleSrc, "config-src", "",
		"override gateway hermes state dir to bundle for phase04 (default: $HOME/.hermes)")
	bootstrapWorkerCmd.Flags().StringVar(&bwEnvGPG, "env-gpg", bwDefaultEnvGPG,
		"gateway-side encrypted env file for phase05")
	bootstrapWorkerCmd.Flags().StringVar(&bwSecretsWrapper, "secrets-wrapper", "",
		"gateway-side run-with-secrets.sh wrapper (default: synthesized inline)")
	bootstrapWorkerCmd.Flags().StringVar(&bwDedicatedKeyPub, "dedicated-key", bwDefaultDedicatedKey,
		"gateway-side dedicated key path (.pub is uploaded; private half stays local)")
	bootstrapWorkerCmd.Flags().StringVar(&bwPlistTemplate, "plist-template", bwDefaultPlistTemplate,
		"gateway-side heartbeat plist template (relative to script-dir parent or absolute)")
	bootstrapWorkerCmd.Flags().StringVar(&bwGatewayPrefix, "gateway-hermes-prefix", bwDefaultGatewayPrefix,
		"gateway-side hermes-state prefix; rewritten to /Users/hermes/.hermes by phase04")
	bootstrapWorkerCmd.Flags().BoolVar(&bwAllowExistAdmin, "allow-existing-admin-user", false,
		"allow a pre-existing 'hermes' user in the admin group (phase02 + phase09)")
	bootstrapWorkerCmd.Flags().BoolVar(&bwSkipTirith, "skip-tirith", false,
		"skip phase07 tirith install (phase09 also skips its tirith probe)")
	bootstrapWorkerCmd.Flags().BoolVar(&bwSkipHeartbeat, "skip-heartbeat", false,
		"skip phase08 heartbeat install (phase09 also skips its heartbeat probe)")
	bootstrapWorkerCmd.Flags().IntVar(&bwHeartbeatWaitSec, "heartbeat-wait-seconds", bwDefaultHeartbeatWait,
		"how long phase08 waits for the first heartbeat tick")

	rootCmd.AddCommand(bootstrapWorkerCmd)
}

// bwOrchestratorFactory is overridable in tests.
var bwOrchestratorFactory = func(target osh.Target, timeout time.Duration) (*bootstrap.Orchestrator, error) {
	dialer := osh.NewDialer(osh.WithConnectTimeout(timeout))
	client, err := dialer.Dial(context.Background(), target)
	if err != nil {
		return nil, fmt.Errorf("bootstrap-worker: dial %s: %w", target, err)
	}
	return &bootstrap.Orchestrator{
		Upload: func(ctx context.Context, localPath, remotePath string) error {
			data, err := os.ReadFile(localPath)
			if err != nil {
				return err
			}
			// Write to remote via ssh `cat > <path>`.
			stdin := strings.NewReader(string(data))
			_ = stdin
			_, _, _, err = func() (string, string, int, error) {
				res, runErr := client.Run(ctx, fmt.Sprintf("cat > %s && chmod +x %s", remotePath, remotePath))
				return res.Stdout, res.Stderr, res.ExitCode, runErr
			}()
			return err
		},
		Run: func(ctx context.Context, remoteCmd string) (string, string, int, error) {
			res, err := client.Run(ctx, remoteCmd)
			return res.Stdout, res.Stderr, res.ExitCode, err
		},
	}, nil
}

// uploadRawFunc is the type the per-phase preparer uses to stage raw
// (non-script) blobs onto the worker. Defaults to the orchestrator's
// Upload field; tests can substitute.
type uploadRawFunc func(ctx context.Context, localPath, remotePath string) error

// phasePrep produces the args + an optional pre-execution side effect
// (e.g. staging a tarball on the worker). The teardown closure runs
// after the phase completes (success or fail).
type phasePrep struct {
	args     []string
	prepare  func(ctx context.Context, upload uploadRawFunc) error
	teardown func()
}

func runBootstrapWorker(cmd *cobra.Command, _ []string) error {
	if bwNode == "" {
		return errors.New("bootstrap-worker: --node is required")
	}

	target, err := osh.ParseTarget(bwNode)
	if err != nil {
		return fmt.Errorf("bootstrap-worker: parse --node: %w", err)
	}

	resolvedLog, err := expandHomePath(bwLogPath)
	if err != nil {
		return fmt.Errorf("bootstrap-worker: expand log path: %w", err)
	}
	logger, err := openLogger(resolvedLog)
	if err != nil {
		return fmt.Errorf("bootstrap-worker: open log: %w", err)
	}
	defer logger.Close()

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	orch, err := bwOrchestratorFactory(target, bwTimeout)
	if err != nil {
		return err
	}
	if bwScriptDir != "" {
		orch.ScriptDir = bwScriptDir
	}

	// Resolve pinned version up front (phase03 + phase09 both need it).
	pinned, err := resolvePinnedVersion()
	if err != nil {
		return err
	}

	phases := bwPhasesToRun()
	if len(phases) == 0 {
		return errors.New("bootstrap-worker: no phase scripts selected")
	}

	nodeLabel := target.User + "@" + target.Host
	overall := log.ResultOK

	for _, phase := range phases {
		prep, perr := buildPhasePrep(phase, nodeLabel, pinned)
		if perr != nil {
			return fmt.Errorf("bootstrap-worker: %s prep: %w", phase, perr)
		}

		phaseCtx, cancel := context.WithTimeout(ctx, bwTimeout)
		if prep.prepare != nil {
			if perr := prep.prepare(phaseCtx, orch.Upload); perr != nil {
				cancel()
				return fmt.Errorf("bootstrap-worker: %s prepare: %w", phase, perr)
			}
		}

		res, runErr := orch.RunPhase(phaseCtx, phase, prep.args...)
		cancel()
		if prep.teardown != nil {
			prep.teardown()
		}

		if !bwQuiet {
			fmt.Fprintf(cmd.OutOrStdout(),
				"phase=%s result=%s duration=%s\n",
				phase, phaseStatus(res, runErr), res.Duration.Round(time.Millisecond))
			res.WriteReport(cmd.OutOrStdout())
		}

		// Write one JSONL event per phase.
		result := log.ResultOK
		tail := ""
		if runErr != nil || !res.OK() {
			result = log.ResultError
			if runErr != nil {
				tail = runErr.Error()
			} else {
				tail = fmt.Sprintf("exit=%d", res.ExitCode)
			}
			overall = log.ResultError
		}
		_ = logger.Action(log.Event{
			Node:       nodeLabel,
			Phase:      "bootstrap",
			Action:     "phase:" + phase,
			Result:     result,
			DurationMs: res.Duration.Milliseconds(),
			StderrTail: tail,
		})

		if runErr != nil {
			return fmt.Errorf("bootstrap-worker: %s: %w", phase, runErr)
		}
		if !res.OK() && !bwDryRun {
			return fmt.Errorf("bootstrap-worker: %s failed (exit=%d)", phase, res.ExitCode)
		}
	}

	if overall != log.ResultOK {
		return errors.New("bootstrap-worker: one or more phases failed")
	}
	if !bwQuiet {
		fmt.Fprintf(cmd.OutOrStdout(), "\nbootstrap-worker: all phases complete for %s\n", nodeLabel)
	}
	return nil
}

func phaseStatus(res bootstrap.PhaseResult, runErr error) string {
	if runErr != nil {
		return "transport-error"
	}
	if res.OK() {
		return "ok"
	}
	return fmt.Sprintf("failed(exit=%d)", res.ExitCode)
}

// bwPhasesToRun returns the ordered list of phase script names to execute.
// When --phase is supplied, runs just that one; otherwise runs the full
// pipeline phase00..phase09 in canonical order.
func bwPhasesToRun() []string {
	if bwPhase != "" {
		return []string{bwPhase}
	}
	out := make([]string, len(phaseList))
	copy(out, phaseList)
	return out
}

// resolvePinnedVersion follows the precedence:
//
//	--pinned-version flag, else
//	first line of --pinned-version-file (default ~/.hermes/PINNED_VERSION).
//
// Returns "" with no error if neither is set AND the file is absent —
// phase00..phase02 don't need it, so we tolerate that for partial runs.
func resolvePinnedVersion() (string, error) {
	if bwPinnedVersion != "" {
		return strings.TrimSpace(bwPinnedVersion), nil
	}
	if bwPinnedFile == "" {
		return "", nil
	}
	path, err := expandHomePath(bwPinnedFile)
	if err != nil {
		return "", fmt.Errorf("pinned-version-file: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line, nil
		}
	}
	return "", nil
}

// buildPhasePrep returns the per-phase arg vector + any pre-execution
// staging steps needed (uploading tarballs, plists, pubkeys, etc.).
func buildPhasePrep(phase, nodeLabel, pinnedVersion string) (phasePrep, error) {
	common := []string{"--node", nodeLabel}
	if bwDryRun {
		common = append(common, "--dry-run")
	}

	switch phase {
	case "phase00_brew_baseline.sh":
		return phasePrep{args: common}, nil

	case "phase01_verify_prereqs.sh":
		return phasePrep{args: common}, nil

	case "phase02_create_user.sh":
		args := append([]string{}, common...)
		if bwAllowExistAdmin {
			args = append(args, "--allow-existing-admin-user")
		}
		return phasePrep{args: args}, nil

	case "phase03_install_hermes.sh":
		if pinnedVersion == "" {
			return phasePrep{}, errors.New("phase03 requires --pinned-version or a non-empty --pinned-version-file")
		}
		args := append([]string{"--pinned-version", pinnedVersion}, common...)
		return phasePrep{args: args}, nil

	case "phase04_seed_config.sh":
		return buildPhase04Prep(common)

	case "phase05_distribute_secrets.sh":
		return buildPhase05Prep(common)

	case "phase06_setup_dedicated_key.sh":
		return buildPhase06Prep(common)

	case "phase07_install_tirith.sh":
		args := append([]string{}, common...)
		if bwSkipTirith {
			args = append(args, "--skip")
		}
		return phasePrep{args: args}, nil

	case "phase08_launchd_heartbeat.sh":
		return buildPhase08Prep(common, nodeLabel)

	case "phase09_verify.sh":
		if pinnedVersion == "" {
			return phasePrep{}, errors.New("phase09 requires --pinned-version or a non-empty --pinned-version-file")
		}
		args := append([]string{"--pinned-version", pinnedVersion}, common...)
		if bwSkipTirith {
			args = append(args, "--skip-tirith")
		}
		if bwSkipHeartbeat {
			args = append(args, "--skip-heartbeat")
		}
		if bwAllowExistAdmin {
			args = append(args, "--allow-existing-admin-user")
		}
		return phasePrep{args: args}, nil
	}

	// Unknown phase — return empty args so RunPhase's "script not found"
	// error path handles it cleanly.
	return phasePrep{args: common}, nil
}

func buildPhase04Prep(common []string) (phasePrep, error) {
	if bwDryRun {
		// Dry-run still requires a bundle arg, but we can point at a
		// dummy path the script will probe-error on — except its
		// dry-run branch reports skipped for the probe. Easier: stage a
		// tiny placeholder tarball.
	}
	srcDir, err := expandHomePath(bwConfigBundleOrDefault())
	if err != nil {
		return phasePrep{}, fmt.Errorf("config-src: %w", err)
	}
	cfg := filepath.Join(srcDir, "config.yaml")
	if _, err := os.Stat(cfg); err != nil {
		return phasePrep{}, fmt.Errorf("phase04: %s not found (set --config-src)", cfg)
	}

	tmp, err := os.CreateTemp("", "phase04-bundle-*.tar.gz")
	if err != nil {
		return phasePrep{}, fmt.Errorf("phase04: temp file: %w", err)
	}
	tmp.Close()
	if err := tarGzipDir(srcDir, tmp.Name(), []string{"config.yaml", "skills", "templates"}); err != nil {
		os.Remove(tmp.Name())
		return phasePrep{}, fmt.Errorf("phase04: tar: %w", err)
	}

	remote := "/tmp/hermes-config-bundle.tar.gz"
	prefix, err := expandHomePath(bwGatewayPrefix)
	if err != nil {
		os.Remove(tmp.Name())
		return phasePrep{}, fmt.Errorf("phase04: gateway-prefix: %w", err)
	}
	args := append([]string{"--config-bundle", remote, "--gateway-hermes-prefix", prefix}, common...)
	return phasePrep{
		args: args,
		prepare: func(ctx context.Context, up uploadRawFunc) error {
			return up(ctx, tmp.Name(), remote)
		},
		teardown: func() { os.Remove(tmp.Name()) },
	}, nil
}

func bwConfigBundleOrDefault() string {
	if bwConfigBundleSrc != "" {
		return bwConfigBundleSrc
	}
	return bwGatewayPrefix
}

func buildPhase05Prep(common []string) (phasePrep, error) {
	envSrc, err := expandHomePath(bwEnvGPG)
	if err != nil {
		return phasePrep{}, fmt.Errorf("phase05: env-gpg: %w", err)
	}
	if _, err := os.Stat(envSrc); err != nil {
		return phasePrep{}, fmt.Errorf("phase05: %s not found", envSrc)
	}

	// Wrapper: if user supplied one, use it; else synthesize.
	var wrapperPath string
	var cleanupWrapper func()
	if bwSecretsWrapper != "" {
		w, err := expandHomePath(bwSecretsWrapper)
		if err != nil {
			return phasePrep{}, fmt.Errorf("phase05: secrets-wrapper: %w", err)
		}
		wrapperPath = w
	} else {
		tmp, err := os.CreateTemp("", "run-with-secrets-*.sh")
		if err != nil {
			return phasePrep{}, fmt.Errorf("phase05: wrapper temp: %w", err)
		}
		_, _ = tmp.WriteString(runWithSecretsTemplate)
		tmp.Close()
		_ = os.Chmod(tmp.Name(), 0o755)
		wrapperPath = tmp.Name()
		cleanupWrapper = func() { os.Remove(tmp.Name()) }
	}

	remoteEnv := "/tmp/.env.gpg"
	remoteWrap := "/tmp/run-with-secrets.sh"
	args := append([]string{"--env-gpg", remoteEnv, "--wrapper", remoteWrap}, common...)
	return phasePrep{
		args: args,
		prepare: func(ctx context.Context, up uploadRawFunc) error {
			if err := up(ctx, envSrc, remoteEnv); err != nil {
				return fmt.Errorf("upload env-gpg: %w", err)
			}
			if err := up(ctx, wrapperPath, remoteWrap); err != nil {
				return fmt.Errorf("upload wrapper: %w", err)
			}
			return nil
		},
		teardown: func() {
			if cleanupWrapper != nil {
				cleanupWrapper()
			}
		},
	}, nil
}

func buildPhase06Prep(common []string) (phasePrep, error) {
	keyPath, err := expandHomePath(bwDedicatedKeyPub)
	if err != nil {
		return phasePrep{}, fmt.Errorf("phase06: dedicated-key: %w", err)
	}
	pub := keyPath + ".pub"
	if _, err := os.Stat(pub); err != nil {
		return phasePrep{}, fmt.Errorf("phase06: pubkey %s not found (generate the keypair on the gateway first)", pub)
	}

	remote := "/tmp/dedicated.pub"
	args := append([]string{"--pubkey", remote}, common...)
	return phasePrep{
		args: args,
		prepare: func(ctx context.Context, up uploadRawFunc) error {
			return up(ctx, pub, remote)
		},
	}, nil
}

func buildPhase08Prep(common []string, nodeLabel string) (phasePrep, error) {
	args := append([]string{}, common...)
	if bwSkipHeartbeat {
		args = append(args, "--skip")
		return phasePrep{args: args}, nil
	}

	tpl, err := resolvePlistTemplate()
	if err != nil {
		return phasePrep{}, fmt.Errorf("phase08: %w", err)
	}
	raw, err := os.ReadFile(tpl)
	if err != nil {
		return phasePrep{}, fmt.Errorf("phase08: read %s: %w", tpl, err)
	}
	// {{HERMES_HOME}} and {{NODE_LABEL}} substitution.
	rendered := strings.ReplaceAll(string(raw), "{{HERMES_HOME}}", "/Users/"+bwDefaultHermesUser)
	host := nodeLabel
	if at := strings.Index(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	rendered = strings.ReplaceAll(rendered, "{{NODE_LABEL}}", host)

	tmp, err := os.CreateTemp("", "heartbeat-*.plist")
	if err != nil {
		return phasePrep{}, fmt.Errorf("phase08: temp file: %w", err)
	}
	_, _ = tmp.WriteString(rendered)
	tmp.Close()

	remote := "/tmp/com.hermes.heartbeat.plist"
	args = append(args, "--plist", remote,
		"--wait-seconds", fmt.Sprintf("%d", bwHeartbeatWaitSec))
	return phasePrep{
		args: args,
		prepare: func(ctx context.Context, up uploadRawFunc) error {
			return up(ctx, tmp.Name(), remote)
		},
		teardown: func() { os.Remove(tmp.Name()) },
	}, nil
}

// resolvePlistTemplate finds the heartbeat plist template. Tries (in
// order): bwPlistTemplate as-is if absolute, then under bwScriptDir's
// parent, then under DefaultScriptDir's parent, then ./templates.
func resolvePlistTemplate() (string, error) {
	if filepath.IsAbs(bwPlistTemplate) {
		if _, err := os.Stat(bwPlistTemplate); err == nil {
			return bwPlistTemplate, nil
		}
		return "", fmt.Errorf("plist-template %s not found", bwPlistTemplate)
	}
	cands := []string{bwPlistTemplate}
	if bwScriptDir != "" {
		cands = append(cands, filepath.Join(filepath.Dir(bwScriptDir), bwPlistTemplate))
	}
	if sd, err := bootstrap.DefaultScriptDir(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(sd), bwPlistTemplate))
	}
	wd, err := os.Getwd()
	if err == nil {
		cands = append(cands, filepath.Join(wd, bwPlistTemplate))
	}
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("plist-template %s not found in any of %v", bwPlistTemplate, cands)
}

// runWithSecretsTemplate is the default decryption wrapper synthesized
// for phase05 when --secrets-wrapper isn't supplied. Mirrors the
// gateway-side prototype's wrapper byte-for-byte.
const runWithSecretsTemplate = `#!/usr/bin/env bash
# run-with-secrets.sh — decrypts .env.gpg into the environment of the
# subsequent command and execs it. Plaintext is never written to disk.
set -euo pipefail
ENV_FILE="${HERMES_ENV_GPG:-$HOME/.hermes/.env.gpg}"
if [ ! -f "$ENV_FILE" ]; then
  echo "run-with-secrets: $ENV_FILE not found" >&2
  exit 1
fi
# shellcheck disable=SC2046
exec env $(gpg -d --batch --quiet "$ENV_FILE" 2>/dev/null \
            | grep -v '^[[:space:]]*#' \
            | grep -v '^[[:space:]]*$') "$@"
`

// tarGzipDir packs the given names (relative to root) into a gzipped
// tarball at dest. Members that don't exist are silently skipped, but
// at least one must succeed. Matches the prototype's tar fallback
// behaviour.
func tarGzipDir(root, dest string, names []string) error {
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	any := false
	for _, name := range names {
		p := filepath.Join(root, name)
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if err := walkTar(tw, root, p, info); err != nil {
			return err
		}
		any = true
	}
	if !any {
		return fmt.Errorf("none of %v existed under %s", names, root)
	}
	return nil
}

func walkTar(tw *tar.Writer, root, path string, info os.FileInfo) error {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel + "/"
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, e := range entries {
			full := filepath.Join(path, e.Name())
			ei, err := e.Info()
			if err != nil {
				return err
			}
			if err := walkTar(tw, root, full, ei); err != nil {
				return err
			}
		}
		return nil
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = rel
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := bytes.Buffer{}
	if _, err := io.Copy(&buf, f); err != nil {
		return err
	}
	if _, err := tw.Write(buf.Bytes()); err != nil {
		return err
	}
	return nil
}

func bootstrapWorkerSkill() Skill {
	return Skill{
		Name:    "bootstrap-worker",
		Trigger: "/bootstrap-worker",
		Summary: "Provision a worker by running the multi-phase bootstrap pipeline (phase00..phase09).",
		Args: []SkillArg{
			{Name: "--node USER@HOST", Description: "Target worker (required)."},
			{Name: "--phase SCRIPT", Description: "Run a single named phase script instead of all 10."},
			{Name: "--script-dir PATH", Description: "Override the local remote/ directory."},
			{Name: "--pinned-version TAG", Description: "Override pinned Hermes version (phase03 + phase09)."},
			{Name: "--pinned-version-file PATH", Description: "File to read pinned version from (default ~/.hermes/PINNED_VERSION)."},
			{Name: "--config-src DIR", Description: "Gateway-side hermes state dir to bundle for phase04 (default ~/.hermes)."},
			{Name: "--env-gpg PATH", Description: "Gateway-side .env.gpg to distribute (phase05)."},
			{Name: "--secrets-wrapper PATH", Description: "Override run-with-secrets.sh wrapper (phase05)."},
			{Name: "--dedicated-key PATH", Description: "Gateway-side dedicated SSH key path; .pub is uploaded (phase06)."},
			{Name: "--plist-template PATH", Description: "Heartbeat plist template (phase08)."},
			{Name: "--gateway-hermes-prefix DIR", Description: "Gateway hermes-state prefix rewritten by phase04 (default ~/.hermes)."},
			{Name: "--allow-existing-admin-user", Description: "Allow pre-existing 'hermes' user in admin group (phase02 + phase09)."},
			{Name: "--skip-tirith", Description: "Skip phase07 + phase09 tirith probe."},
			{Name: "--skip-heartbeat", Description: "Skip phase08 + phase09 heartbeat probe."},
			{Name: "--heartbeat-wait-seconds N", Description: "First-heartbeat wait window (phase08)."},
			{Name: "--dry-run", Description: "Pass --dry-run to phase scripts (probe only, no mutations)."},
			{Name: "--timeout DUR", Description: "Per-phase timeout (default 10m)."},
			{Name: "--quiet", Description: "Suppress stdout; JSONL log still written."},
		},
		Examples: []SkillExample{
			{Description: "Full bootstrap (all 10 phases)", Command: "fleetctl bootstrap-worker --node hermes@claw1.local"},
			{Description: "Dry-run probe", Command: "fleetctl bootstrap-worker --node hermes@claw1.local --dry-run"},
			{Description: "Single phase", Command: "fleetctl bootstrap-worker --node hermes@claw1.local --phase phase09_verify.sh"},
		},
	}
}

func init() { registerSkill("bootstrap-worker", bootstrapWorkerSkill) }
