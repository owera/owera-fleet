// config_sync.go implements `fleetctl config-sync` — sign and push config
// bundles from gateway to worker nodes, or verify bundles on the worker side.
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

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/configsync"
)

var (
	csKeyDir     string
	csBundleID   string
	csSrcDir     string
	csOut        string
	csIn         string
	csDest       string
	csVerifyOnly bool
)

var configSyncCmd = &cobra.Command{
	Use:   "config-sync",
	Short: "Sign and push config bundles to worker nodes",
	Long: `config-sync packages a directory of config files into a signed tar
bundle, transferring trust from the gateway to workers via a pinned ed25519
public key. Each bundle's manifest is signed; workers verify the signature
and every file's hash before applying.

Subcommands:
  init    Generate or read the gateway signing key pair.
  pack    Create a signed bundle from a source directory.
  verify  Verify a bundle's signature and file hashes (no extraction).
  apply   Verify and unpack a bundle into a destination directory.`,
}

var configSyncInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Generate the gateway signing key pair (idempotent)",
	RunE:  runConfigSyncInit,
}

var configSyncPackCmd = &cobra.Command{
	Use:   "pack",
	Short: "Create a signed bundle from a source directory",
	RunE:  runConfigSyncPack,
}

var configSyncVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Verify a signed bundle without extracting it",
	RunE:  runConfigSyncVerify,
}

var configSyncApplyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Verify and apply a signed bundle to a destination directory",
	RunE:  runConfigSyncApply,
}

func init() {
	configSyncCmd.PersistentFlags().StringVar(&csKeyDir, "key-dir", "~/.hermes/configsync", "signing key directory")
	configSyncPackCmd.Flags().StringVar(&csSrcDir, "src", "", "source config directory (required)")
	configSyncPackCmd.Flags().StringVar(&csOut, "out", "", "output bundle path (required)")
	configSyncPackCmd.Flags().StringVar(&csBundleID, "bundle-id", "", "bundle identifier (default: basename of src)")
	configSyncVerifyCmd.Flags().StringVar(&csIn, "in", "", "bundle path (required)")
	configSyncApplyCmd.Flags().StringVar(&csIn, "in", "", "bundle path (required)")
	configSyncApplyCmd.Flags().StringVar(&csDest, "dest", "", "destination directory (required)")
	configSyncApplyCmd.Flags().BoolVar(&csVerifyOnly, "verify-only", false, "verify but do not extract")

	configSyncCmd.AddCommand(configSyncInitCmd, configSyncPackCmd, configSyncVerifyCmd, configSyncApplyCmd)
	rootCmd.AddCommand(configSyncCmd)
}

func runConfigSyncInit(cmd *cobra.Command, _ []string) error {
	dir, err := expandHomePath(csKeyDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config-sync init: mkdir: %w", err)
	}
	privPath := filepath.Join(dir, "signing.key")
	pubPath := filepath.Join(dir, "signing.pub")

	if _, err := os.Stat(privPath); err == nil {
		fmt.Fprintln(cmd.OutOrStdout(), "Key pair already exists at", dir)
		return nil
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("config-sync init: generate: %w", err)
	}
	if err := os.WriteFile(privPath, []byte(base64.StdEncoding.EncodeToString(priv)), 0o600); err != nil {
		return fmt.Errorf("config-sync init: write priv: %w", err)
	}
	if err := os.WriteFile(pubPath, []byte(base64.StdEncoding.EncodeToString(pub)), 0o644); err != nil {
		return fmt.Errorf("config-sync init: write pub: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Generated key pair:\n  Private: %s\n  Public:  %s\n", privPath, pubPath)
	fmt.Fprintln(cmd.OutOrStdout(), "Distribute signing.pub to every worker for verify-side trust.")
	return nil
}

func runConfigSyncPack(cmd *cobra.Command, _ []string) error {
	if csSrcDir == "" || csOut == "" {
		return errors.New("config-sync pack: --src and --out are required")
	}
	dir, err := expandHomePath(csKeyDir)
	if err != nil {
		return err
	}
	signer, err := configsync.LoadSigner(filepath.Join(dir, "signing.key"))
	if err != nil {
		return err
	}
	bundleID := csBundleID
	if bundleID == "" {
		bundleID = filepath.Base(strings.TrimRight(csSrcDir, "/"))
	}
	if err := signer.PackDir(csSrcDir, csOut, bundleID); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Packed bundle %q from %s to %s\n", bundleID, csSrcDir, csOut)
	return nil
}

func runConfigSyncVerify(cmd *cobra.Command, _ []string) error {
	if csIn == "" {
		return errors.New("config-sync verify: --in is required")
	}
	dir, err := expandHomePath(csKeyDir)
	if err != nil {
		return err
	}
	verifier, err := configsync.LoadVerifier(filepath.Join(dir, "signing.pub"))
	if err != nil {
		return err
	}
	m, err := verifier.Verify(csIn)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Bundle %q verified (%d files, signed at %s)\n",
		m.BundleID, len(m.Files), m.CreatedAt.Format("2006-01-02T15:04:05Z"))
	return nil
}

func runConfigSyncApply(cmd *cobra.Command, _ []string) error {
	if csIn == "" {
		return errors.New("config-sync apply: --in is required")
	}
	dir, err := expandHomePath(csKeyDir)
	if err != nil {
		return err
	}
	verifier, err := configsync.LoadVerifier(filepath.Join(dir, "signing.pub"))
	if err != nil {
		return err
	}
	if csVerifyOnly {
		m, err := verifier.Verify(csIn)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Bundle %q verified (%d files)\n", m.BundleID, len(m.Files))
		return nil
	}
	if csDest == "" {
		return errors.New("config-sync apply: --dest is required")
	}
	m, err := verifier.Apply(csIn, csDest)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Applied bundle %q to %s (%d files)\n", m.BundleID, csDest, len(m.Files))
	return nil
}

func configSyncSkill() Skill {
	return Skill{
		Name:    "config-sync",
		Trigger: "/config-sync",
		Summary: "Sign and push config bundles to worker nodes; verify with pinned ed25519 public key.",
		Args: []SkillArg{
			{Name: "init", Description: "Generate the gateway signing key pair."},
			{Name: "pack --src DIR --out FILE", Description: "Create a signed bundle."},
			{Name: "verify --in FILE", Description: "Verify a bundle without extracting."},
			{Name: "apply --in FILE --dest DIR", Description: "Verify and unpack a bundle."},
		},
		Examples: []SkillExample{
			{Description: "Init key pair", Command: "fleetctl config-sync init"},
			{Description: "Pack hermes config", Command: "fleetctl config-sync pack --src ~/.hermes/config --out /tmp/bundle.tar"},
			{Description: "Apply on worker", Command: "fleetctl config-sync apply --in bundle.tar --dest ~/.hermes/config"},
		},
	}
}

func init() { registerSkill("config-sync", configSyncSkill) }
