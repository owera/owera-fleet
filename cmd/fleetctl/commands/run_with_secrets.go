// run_with_secrets.go implements `fleetctl run-with-secrets` — executes a
// command with secrets delivered via stdin rather than environment variables.
package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/secrets"
)

var (
	rwsEnvFile  string
	rwsSecrets  []string
	rwsUseEnv   bool
)

var runWithSecretsCmd = &cobra.Command{
	Use:   "run-with-secrets -- <cmd> [args...]",
	Short: "Execute a command with secrets delivered via stdin",
	Long: `run-with-secrets executes a command with secrets piped to stdin as
KEY=value lines (NUL-delimited), never via environment variables or argv.

Secrets are specified as KEY=value pairs via --secret or loaded from a
.env file with --env-file. Shell scripts read them with:

    eval "$(head -c -1 /dev/stdin | tr '\000' '\n')"

Use --use-env to instead inject secrets directly as environment variables
(opt-in; appropriate for trusted binaries that expect env vars).`,
	Example: `  fleetctl run-with-secrets --secret TOKEN=abc123 -- sh ./deploy.sh
  fleetctl run-with-secrets --env-file .env -- python task.py
  fleetctl run-with-secrets --use-env --secret API_KEY=xyz -- curl ...`,
	RunE: runWithSecrets,
}

func init() {
	runWithSecretsCmd.Flags().StringVar(&rwsEnvFile, "env-file", "", "load secrets from a KEY=value file")
	runWithSecretsCmd.Flags().StringSliceVar(&rwsSecrets, "secret", nil, "KEY=value secret (repeatable)")
	runWithSecretsCmd.Flags().BoolVar(&rwsUseEnv, "use-env", false, "inject secrets as env vars instead of stdin")
	rootCmd.AddCommand(runWithSecretsCmd)
}

// runWithSecretsRunner is overridable in tests.
var runWithSecretsRunner = func(ctx context.Context, cfg secrets.RunConfig) (secrets.Result, error) {
	return secrets.Run(ctx, cfg)
}

var runWithSecretsEnvRunner = func(ctx context.Context, cmd []string, sec map[string]string, env []string) (secrets.Result, error) {
	return secrets.RunWithEnv(ctx, cmd, sec, env)
}

func runWithSecrets(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return errors.New("run-with-secrets: command is required (after --)")
	}

	sec, err := loadSecrets(rwsSecrets, rwsEnvFile)
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	if rwsUseEnv {
		res, err := runWithSecretsEnvRunner(ctx, args, sec, nil)
		if err != nil {
			return err
		}
		fmt.Fprint(cmd.OutOrStdout(), res.Stdout)
		fmt.Fprint(cmd.ErrOrStderr(), res.Stderr)
		if res.ExitCode != 0 {
			return fmt.Errorf("run-with-secrets: exited %d", res.ExitCode)
		}
		return nil
	}

	res, err := runWithSecretsRunner(ctx, secrets.RunConfig{
		Cmd:     args,
		Secrets: sec,
		Stdout:  cmd.OutOrStdout(),
		Stderr:  cmd.ErrOrStderr(),
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("run-with-secrets: exited %d", res.ExitCode)
	}
	return nil
}

// loadSecrets merges --secret flags and --env-file into one map.
func loadSecrets(flags []string, envFile string) (map[string]string, error) {
	sec := make(map[string]string)
	for _, kv := range flags {
		idx := strings.IndexByte(kv, '=')
		if idx <= 0 {
			return nil, fmt.Errorf("run-with-secrets: invalid --secret %q (want KEY=value)", kv)
		}
		sec[kv[:idx]] = kv[idx+1:]
	}
	if envFile != "" {
		data, err := os.ReadFile(envFile)
		if err != nil {
			return nil, fmt.Errorf("run-with-secrets: read env-file: %w", err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			idx := strings.IndexByte(line, '=')
			if idx <= 0 {
				continue
			}
			key := line[:idx]
			if _, exists := sec[key]; !exists { // --secret overrides env-file
				sec[key] = line[idx+1:]
			}
		}
	}
	return sec, nil
}

func runWithSecretsSkill() Skill {
	return Skill{
		Name:    "run-with-secrets",
		Trigger: "/run-with-secrets",
		Summary: "Execute a command with secrets delivered via stdin (never via env vars or argv).",
		Args: []SkillArg{
			{Name: "--secret KEY=VALUE", Description: "Secret key-value pair (repeatable)."},
			{Name: "--env-file FILE", Description: "Load secrets from a KEY=value file."},
			{Name: "--use-env", Description: "Inject secrets as env vars instead of stdin (opt-in)."},
		},
		Examples: []SkillExample{
			{Description: "Deliver token via stdin", Command: "fleetctl run-with-secrets --secret TOKEN=abc123 -- sh ./deploy.sh"},
			{Description: "Load from .env file", Command: "fleetctl run-with-secrets --env-file .env -- python task.py"},
		},
	}
}

func init() { registerSkill("run-with-secrets", runWithSecretsSkill) }
