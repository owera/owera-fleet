// skill_sync.go implements `fleetctl skill-sync` — sign skill bundles on
// the gateway and pull/verify/apply them on worker nodes from a remote
// signed-Git source.
//
// This is the §4 signed-skill-sync primitive, the agent plane's counterpart
// to config-sync. Subcommands:
//
//	init    Generate the gateway signing key pair under ~/.hermes/skillsync/
//	sign    Walk a source directory, hash its files, write a signed manifest
//	pull    Clone (or fetch), verify, and optionally apply the bundle
//	verify  Clone and verify only — never touches the apply destination
//
// Every invocation appends one JSONL event to ~/.hermes/logs/skill-sync.jsonl
// via internal/log, with phase="skill-sync" and an action describing the
// operation. The audit/state subcommands consume that stream alongside the
// rest of the fleet telemetry.
package commands

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/log"
	"github.com/owera/owera-fleet/internal/skillsync"
)

// skillSyncLogPath is the canonical JSONL log location, mirroring the
// delegate.jsonl pattern. Overridable in tests.
var skillSyncLogPath = "~/.hermes/logs/skill-sync.jsonl"

// skillSyncOpenLogger is overridable in tests; production wraps log.Open.
var skillSyncOpenLogger = func(path string) (*log.Logger, error) { return log.Open(path) }

// skillSyncNewPuller is overridable in tests so we don't shell out to git.
// In production it returns a Puller with skillsync.DefaultGitRunner.
var skillSyncNewPuller = func(src skillsync.Source, localDir string) *skillsync.Puller {
	return &skillsync.Puller{
		GitRunner: skillsync.DefaultGitRunner,
		Source:    src,
		LocalDir:  localDir,
	}
}

// skillSyncDefaultDest is the on-worker destination root for applied
// skills. The agentic-services subdir matches what `bootstrap-hermes-node.sh`
// places under ~/.claude/skills/.
const skillSyncDefaultDest = "~/.claude/skills/agentic-services"

var (
	ssKeyDir   string
	ssSrcDir   string
	ssOutFile  string
	ssBundleID string
	ssGitURL   string
	ssBranch   string
	ssPubKey   string
	ssSubDir   string
	ssLocalDir string
	ssDest     string
	ssApply    bool
)

var skillSyncCmd = &cobra.Command{
	Use:   "skill-sync",
	Short: "Sign and pull skill bundles to worker nodes via a signed-Git source",
	Long: `skill-sync delivers per-skill bundles from the gateway to workers via a
remote Git repository signed with a pinned ed25519 key. The gateway runs
sign to produce .skills-manifest.json + .skills-signature inside the
bundle; workers pull, verify the signature + every file hash, and apply
atomically to ~/.claude/skills/agentic-services/.

Subcommands:
  init    Generate the gateway signing key pair (idempotent).
  sign    Hash a source directory and write a signed manifest into it.
  pull    Clone or fetch the remote, verify, and (with --apply) write the
          skills to the destination directory.
  verify  Clone and verify only — never writes to the apply destination.`,
}

var skillSyncInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Generate the gateway signing key pair (idempotent)",
	RunE:  runSkillSyncInit,
}

var skillSyncSignCmd = &cobra.Command{
	Use:   "sign",
	Short: "Hash a source directory and write a signed manifest inside it",
	RunE:  runSkillSyncSign,
}

var skillSyncPullCmd = &cobra.Command{
	Use:   "pull",
	Short: "Clone or fetch the signed-Git skill source, verify, and optionally apply",
	RunE:  runSkillSyncPull,
}

var skillSyncVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Clone and verify the signed-Git skill source without applying",
	RunE:  runSkillSyncVerify,
}

func init() {
	skillSyncCmd.PersistentFlags().StringVar(&ssKeyDir, "key-dir", "~/.hermes/skillsync", "signing key directory")

	skillSyncSignCmd.Flags().StringVar(&ssSrcDir, "src", "", "source skills directory (required)")
	// --out names the manifest filename written inside --src; defaults to
	// .skills-manifest.json. Kept as a flag so a future on-disk migration
	// can be done without breaking the documented surface.
	skillSyncSignCmd.Flags().StringVar(&ssOutFile, "out", skillsync.ManifestName, "manifest filename written inside --src (advisory; signer always writes "+skillsync.ManifestName+")")
	skillSyncSignCmd.Flags().StringVar(&ssBundleID, "bundle-id", "", "bundle identifier (default: basename of src + UTC timestamp)")

	for _, c := range []*cobra.Command{skillSyncPullCmd, skillSyncVerifyCmd} {
		c.Flags().StringVar(&ssGitURL, "git-url", "", "remote Git URL (required)")
		c.Flags().StringVar(&ssBranch, "branch", "main", "remote branch")
		c.Flags().StringVar(&ssPubKey, "pub-key", "", "base64 ed25519 public key (default: read signing.pub from --key-dir)")
		c.Flags().StringVar(&ssSubDir, "skills-dir", "skills", "path within the repo containing the bundle")
		c.Flags().StringVar(&ssLocalDir, "local-dir", "~/.hermes/skillsync/checkout", "persistent local working tree for the clone")
	}
	skillSyncPullCmd.Flags().StringVar(&ssDest, "dest", skillSyncDefaultDest, "destination directory for --apply")
	skillSyncPullCmd.Flags().BoolVar(&ssApply, "apply", false, "after verifying, write the skill files to --dest")

	skillSyncCmd.AddCommand(skillSyncInitCmd, skillSyncSignCmd, skillSyncPullCmd, skillSyncVerifyCmd)
	rootCmd.AddCommand(skillSyncCmd)
}

func runSkillSyncInit(cmd *cobra.Command, _ []string) error {
	logger, closeLog := openSkillSyncLogger(cmd)
	defer closeLog()
	start := time.Now()

	dir, err := expandHomePath(ssKeyDir)
	if err != nil {
		return logSkillSyncErr(logger, "init", start, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return logSkillSyncErr(logger, "init", start, fmt.Errorf("skill-sync init: mkdir: %w", err))
	}
	privPath := filepath.Join(dir, "signing.key")
	pubPath := filepath.Join(dir, "signing.pub")

	if _, statErr := os.Stat(privPath); statErr == nil {
		_ = logger.Action(log.Event{
			Phase:      "skill-sync",
			Action:     "init",
			Result:     log.ResultNoChange,
			DurationMs: time.Since(start).Milliseconds(),
		})
		fmt.Fprintln(cmd.OutOrStdout(), "Key pair already exists at", dir)
		return nil
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return logSkillSyncErr(logger, "init", start, fmt.Errorf("skill-sync init: generate: %w", err))
	}
	if err := os.WriteFile(privPath, []byte(base64.StdEncoding.EncodeToString(priv)), 0o600); err != nil {
		return logSkillSyncErr(logger, "init", start, fmt.Errorf("skill-sync init: write priv: %w", err))
	}
	if err := os.WriteFile(pubPath, []byte(base64.StdEncoding.EncodeToString(pub)), 0o644); err != nil {
		return logSkillSyncErr(logger, "init", start, fmt.Errorf("skill-sync init: write pub: %w", err))
	}
	_ = logger.Action(log.Event{
		Phase:      "skill-sync",
		Action:     "init",
		Result:     log.ResultOK,
		DurationMs: time.Since(start).Milliseconds(),
	})
	fmt.Fprintf(cmd.OutOrStdout(), "Generated key pair:\n  Private: %s\n  Public:  %s\n", privPath, pubPath)
	fmt.Fprintln(cmd.OutOrStdout(), "Distribute signing.pub to every worker for verify-side trust.")
	return nil
}

func runSkillSyncSign(cmd *cobra.Command, _ []string) error {
	logger, closeLog := openSkillSyncLogger(cmd)
	defer closeLog()
	start := time.Now()

	if ssSrcDir == "" {
		return logSkillSyncErr(logger, "sign", start, errors.New("skill-sync sign: --src is required"))
	}
	dir, err := expandHomePath(ssKeyDir)
	if err != nil {
		return logSkillSyncErr(logger, "sign", start, err)
	}
	signer, err := skillsync.LoadSigner(filepath.Join(dir, "signing.key"))
	if err != nil {
		return logSkillSyncErr(logger, "sign", start, err)
	}
	bundleID := ssBundleID
	if bundleID == "" {
		bundleID = filepath.Base(strings.TrimRight(ssSrcDir, "/")) + "-" + time.Now().UTC().Format("20060102T150405Z")
	}
	m, err := signer.SignDir(ssSrcDir, bundleID)
	if err != nil {
		return logSkillSyncErr(logger, "sign "+bundleID, start, err)
	}
	_ = logger.Action(log.Event{
		Phase:      "skill-sync",
		Action:     "sign " + bundleID,
		Result:     log.ResultOK,
		DurationMs: time.Since(start).Milliseconds(),
	})
	fmt.Fprintf(cmd.OutOrStdout(), "Signed bundle %q (%d files) under %s\n", bundleID, len(m.Files), ssSrcDir)
	return nil
}

func runSkillSyncPull(cmd *cobra.Command, _ []string) error {
	return runSkillSyncPullOrVerify(cmd, true)
}

func runSkillSyncVerify(cmd *cobra.Command, _ []string) error {
	return runSkillSyncPullOrVerify(cmd, false)
}

// runSkillSyncPullOrVerify is shared between the pull and verify
// subcommands. allowApply gates whether --apply (and a non-empty --dest)
// causes the verified bundle to be written to disk.
func runSkillSyncPullOrVerify(cmd *cobra.Command, allowApply bool) error {
	op := "pull"
	if !allowApply {
		op = "verify"
	}
	logger, closeLog := openSkillSyncLogger(cmd)
	defer closeLog()
	start := time.Now()

	if ssGitURL == "" {
		return logSkillSyncErr(logger, op, start, errors.New("skill-sync "+op+": --git-url is required"))
	}
	pubKey, err := resolvePubKey()
	if err != nil {
		return logSkillSyncErr(logger, op, start, err)
	}
	localDir, err := expandHomePath(ssLocalDir)
	if err != nil {
		return logSkillSyncErr(logger, op, start, err)
	}

	puller := skillSyncNewPuller(skillsync.Source{
		GitURL:    ssGitURL,
		Branch:    ssBranch,
		PubKey:    pubKey,
		SkillsDir: ssSubDir,
	}, localDir)

	parent := cmd.Context()
	m, err := puller.Pull(parent)
	if err != nil {
		return logSkillSyncErr(logger, op+" "+ssGitURL, start, err)
	}

	if !allowApply || !ssApply {
		_ = logger.Action(log.Event{
			Phase:      "skill-sync",
			Action:     op + " " + m.BundleID,
			Result:     log.ResultOK,
			DurationMs: time.Since(start).Milliseconds(),
		})
		fmt.Fprintf(cmd.OutOrStdout(), "Verified bundle %q (%d files, signed at %s)\n",
			m.BundleID, len(m.Files), m.CreatedAt.Format(time.RFC3339))
		return nil
	}

	destDir, err := expandHomePath(ssDest)
	if err != nil {
		return logSkillSyncErr(logger, "apply", start, err)
	}
	applied, unchanged, err := puller.Apply(parent, destDir)
	if err != nil {
		return logSkillSyncErr(logger, "apply "+m.BundleID, start, err)
	}
	// no-change result if every file was already at the target hash; ok
	// otherwise. This makes a cron-driven pull --apply self-quiet on the
	// audit log when nothing actually changed.
	res := log.ResultOK
	if applied == 0 && unchanged > 0 {
		res = log.ResultNoChange
	}
	_ = logger.Action(log.Event{
		Phase:      "skill-sync",
		Action:     "apply " + m.BundleID,
		Result:     res,
		DurationMs: time.Since(start).Milliseconds(),
	})
	fmt.Fprintf(cmd.OutOrStdout(), "Applied bundle %q to %s: %d written, %d unchanged\n",
		m.BundleID, destDir, applied, unchanged)
	return nil
}

// resolvePubKey returns the base64 ed25519 public key the puller should
// pin. Either taken verbatim from --pub-key or read off disk from
// <key-dir>/signing.pub.
func resolvePubKey() ([]byte, error) {
	if ssPubKey != "" {
		return []byte(ssPubKey), nil
	}
	dir, err := expandHomePath(ssKeyDir)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "signing.pub"))
	if err != nil {
		return nil, fmt.Errorf("skill-sync: read pubkey: %w (run 'fleetctl skill-sync init' or pass --pub-key)", err)
	}
	return data, nil
}

// openSkillSyncLogger opens the JSONL log file and returns the logger + a
// close function. On open failure we fall back to a no-op writer (logged
// to stderr) so the operator-facing command still works even if the log
// dir is unwritable.
func openSkillSyncLogger(cmd *cobra.Command) (*log.Logger, func()) {
	resolved, err := expandHomePath(skillSyncLogPath)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "skill-sync: expand log path: %v\n", err)
		return log.New(devNullWriter{}), func() {}
	}
	logger, err := skillSyncOpenLogger(resolved)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "skill-sync: open log: %v\n", err)
		return log.New(devNullWriter{}), func() {}
	}
	return logger, func() { _ = logger.Close() }
}

// logSkillSyncErr writes an error-result JSONL line and returns the
// original error verbatim so cobra surfaces it to the operator.
func logSkillSyncErr(logger *log.Logger, action string, start time.Time, err error) error {
	tail := err.Error()
	if len(tail) > 4096 {
		tail = tail[len(tail)-4096:]
	}
	_ = logger.Action(log.Event{
		Phase:      "skill-sync",
		Action:     action,
		Result:     log.ResultError,
		DurationMs: time.Since(start).Milliseconds(),
		StderrTail: tail,
	})
	return err
}

// devNullWriter discards writes when no real log file is available. Local
// to this file so it doesn't collide with the (unexported) io/ioutil
// equivalents elsewhere in the package.
type devNullWriter struct{}

func (devNullWriter) Write(p []byte) (int, error) { return len(p), nil }

func skillSyncSkill() Skill {
	return Skill{
		Name:    "skill-sync",
		Trigger: "/skill-sync",
		Summary: "Sign and pull skill bundles to workers via a signed-Git source verified by a pinned ed25519 public key.",
		Args: []SkillArg{
			{Name: "init", Description: "Generate the gateway signing key pair under ~/.hermes/skillsync/."},
			{Name: "sign --src DIR", Description: "Hash DIR and write a signed manifest inside it."},
			{Name: "pull --git-url URL [--apply]", Description: "Clone or fetch the remote, verify, and optionally apply to ~/.claude/skills/agentic-services/."},
			{Name: "verify --git-url URL", Description: "Clone and verify only; do not apply."},
			{Name: "--key-dir DIR", Description: "Override signing key directory (default ~/.hermes/skillsync)."},
			{Name: "--branch NAME", Description: "Remote branch (default main)."},
			{Name: "--skills-dir PATH", Description: "Path within the repo containing the bundle (default skills)."},
			{Name: "--pub-key BASE64", Description: "Override the pinned public key (default: signing.pub in --key-dir)."},
			{Name: "--local-dir DIR", Description: "Persistent local working tree (default ~/.hermes/skillsync/checkout)."},
			{Name: "--dest DIR", Description: "Apply destination (default ~/.claude/skills/agentic-services)."},
		},
		Examples: []SkillExample{
			{Description: "Init signing key on the gateway", Command: "fleetctl skill-sync init"},
			{Description: "Sign a skills directory", Command: "fleetctl skill-sync sign --src ./skills --bundle-id v1"},
			{Description: "Pull and apply on a worker", Command: "fleetctl skill-sync pull --git-url https://github.com/owera/owera-fleet.git --apply"},
			{Description: "Verify only (no apply)", Command: "fleetctl skill-sync verify --git-url https://github.com/owera/owera-fleet.git"},
		},
	}
}

func init() { registerSkill("skill-sync", skillSyncSkill) }
