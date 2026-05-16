// research.go implements `fleetctl research` — delegates a research prompt to
// the Hermes agent (with optional web toolset) and saves the response.
package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/log"
)

var (
	researchPrompt    string
	researchOut       string
	researchWeb       bool
	researchJSON      bool
	researchQuiet     bool
	researchHermesDir string
	researchLogPath   = "~/.hermes/logs/research.jsonl"
)

// researchRunner is overridable in tests; production calls hermes.Client.RunZ.
var researchRunner = func(ctx context.Context, prompt string, web bool) (string, error) {
	c, err := researchHermesClient()
	if err != nil {
		return "", err
	}
	_ = c
	return "", errors.New("research: hermes integration not wired in this build; override researchRunner in tests")
}

// researchHermesClient is a placeholder for production Hermes wiring.
func researchHermesClient() (interface{}, error) {
	return nil, nil
}

var researchCmd = &cobra.Command{
	Use:   "research",
	Short: "Delegate a research query to the Hermes agent",
	Long: `research sends a natural-language prompt to the local Hermes agent and
captures the response as a markdown report. Optionally enables the web
toolset so Hermes can browse for current information.

The research event is appended to ~/.hermes/logs/research.jsonl.`,
	Example: `  fleetctl research --prompt "What is the Go 1.24 release date?"
  fleetctl research --prompt "Current state of SSH agent forwarding on macOS" --web
  fleetctl research --prompt "Summarise the restic changelog" --out /tmp/research.md`,
	RunE: runResearch,
}

func init() {
	researchCmd.Flags().StringVar(&researchPrompt, "prompt", "", "research query (required)")
	researchCmd.Flags().StringVar(&researchOut, "out", "", "write report to FILE (default: stdout)")
	researchCmd.Flags().BoolVar(&researchWeb, "web", false, "enable Hermes web toolset for browsing")
	researchCmd.Flags().BoolVar(&researchJSON, "json", false, "emit JSON instead of markdown")
	researchCmd.Flags().BoolVar(&researchQuiet, "quiet", false, "suppress stdout; JSONL log still written")
	researchCmd.Flags().StringVar(&researchHermesDir, "hermes-dir", "", "override ~/.hermes path")
	rootCmd.AddCommand(researchCmd)
}

func runResearch(cmd *cobra.Command, _ []string) error {
	if strings.TrimSpace(researchPrompt) == "" {
		return errors.New("research: --prompt is required")
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	start := time.Now()
	response, runErr := researchRunner(ctx, researchPrompt, researchWeb)
	dur := time.Since(start).Milliseconds()

	// Write JSONL audit event.
	logSrc := researchLogPath
	if researchHermesDir != "" {
		logSrc = researchHermesDir + "/logs/research.jsonl"
	}
	if resolvedLog, err := expandHomePath(logSrc); err == nil {
		if logger, err := openLogger(resolvedLog); err == nil {
			defer logger.Close()
			node, _ := os.Hostname()
			result := log.ResultOK
			tail := ""
			if runErr != nil {
				result = log.ResultError
				tail = runErr.Error()
			}
			action := "research:" + truncateAction(researchPrompt)
			_ = logger.Action(log.Event{
				Node:       node,
				Phase:      "research",
				Action:     action,
				Result:     result,
				DurationMs: dur,
				StderrTail: tail,
			})
		}
	}

	if runErr != nil {
		return fmt.Errorf("research: hermes run failed: %w", runErr)
	}

	report := buildResearchReport(researchPrompt, response)

	if !researchQuiet {
		if researchOut != "" {
			if err := os.WriteFile(researchOut, []byte(report), 0o644); err != nil {
				return fmt.Errorf("research: write %s: %w", researchOut, err)
			}
		} else {
			fmt.Fprint(cmd.OutOrStdout(), report)
		}
	}
	return nil
}

func buildResearchReport(prompt, response string) string {
	title := prompt
	if len(title) > 60 {
		title = title[:60] + "…"
	}
	var b strings.Builder
	b.WriteString("# Research: ")
	b.WriteString(title)
	b.WriteString("\n\n")
	b.WriteString("Generated: ")
	b.WriteString(time.Now().UTC().Format(time.RFC3339))
	b.WriteString("\n\n")
	b.WriteString(response)
	if !strings.HasSuffix(response, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

func researchSkill() Skill {
	return Skill{
		Name:    "research",
		Trigger: "/research",
		Summary: "Delegate a research query to the Hermes agent and capture the response as a markdown report.",
		Args: []SkillArg{
			{Name: "--prompt TEXT", Description: "Research query (required)."},
			{Name: "--web", Description: "Enable Hermes web toolset for live browsing."},
			{Name: "--out FILE", Description: "Write report to FILE (default: stdout)."},
			{Name: "--quiet", Description: "Suppress stdout; JSONL log still written."},
			{Name: "--hermes-dir PATH", Description: "Override ~/.hermes path."},
		},
		Examples: []SkillExample{
			{Description: "Research a topic", Command: `fleetctl research --prompt "Go 1.24 release notes"`},
			{Description: "Research with web browsing", Command: `fleetctl research --prompt "Latest restic changelog" --web`},
			{Description: "Save to file", Command: `fleetctl research --prompt "SSH agent forwarding macOS" --out /tmp/r.md`},
		},
	}
}

func init() { registerSkill("research", researchSkill) }
