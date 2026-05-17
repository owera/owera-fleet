// markers.go implements `fleetctl markers` — content-hashed marker
// subcommands that expose the internal/markers package as a CLI surface.
//
// The internal package implements crash-safe Write/Read/IsFresh/Invalidate/
// List/ListAll plus a sha256 input hasher. The CLI here is the thin
// operator-facing wrapper that lets shell pipelines (notably the
// usecase34_etl scenario) record idempotency markers without resorting to
// raw shasum/awk pipelines.
//
// Storage layout matches the package: <markers-dir>/<pipeline>/<step>.json.
// Default markers-dir is ~/.hermes/markers; --markers-dir overrides it.
package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/markers"
)

var (
	markersDir      string
	markersPipeline string
	markersStep     string
	markersInput    string
	markersOutput   string
	markersFormat   string
)

var markersCmd = &cobra.Command{
	Use:   "markers",
	Short: "Record and verify content-hashed step markers for idempotent pipelines",
	Long: `markers wraps the internal/markers package as a CLI surface. Use it from
shell scenarios (e.g. ETL harnesses) to record that a step completed with
a particular input hash, then re-check on the next run to make the step a
no-op when the input hasn't changed.

Markers live under <markers-dir>/<pipeline>/<step>.json (default
markers-dir: ~/.hermes/markers).`,
}

var markersHashCmd = &cobra.Command{
	Use:   "hash",
	Short: "Hash an input and write a marker for (pipeline, step)",
	Long: `hash reads the input (file path or "-" for stdin), computes its
sha256, and writes a marker recording the (pipeline, step, input_hash,
completed_at). The marker is the content-hash receipt that lets a future
'markers verify' call skip the step idempotently.`,
	Example: `  fleetctl markers hash --pipeline etl --step load_users --input data.csv
  cat data.csv | fleetctl markers hash --pipeline etl --step load_users --input -`,
	RunE: runMarkersHash,
}

var markersVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Verify a marker is fresh against the current input (exit 0 fresh, 1 stale/missing)",
	Long: `verify hashes the current input and compares it against the stored
marker for (pipeline, step). Exits 0 if the marker is fresh (hashes match),
1 if the marker is missing or stale. Designed for use in shell scenarios:

  if fleetctl markers verify --pipeline etl --step load_users --input f; then
      echo skip
  else
      run_step && fleetctl markers hash --pipeline etl --step load_users --input f
  fi`,
	Example: `  fleetctl markers verify --pipeline etl --step load_users --input data.csv`,
	RunE:    runMarkersVerify,
}

var markersListCmd = &cobra.Command{
	Use:   "list",
	Short: "List markers (all pipelines, or filtered by --pipeline)",
	Long: `list prints the markers currently on disk. With --pipeline, only
markers under that pipeline are shown; without it, every pipeline is
listed. Use --format=json for machine-readable output.`,
	Example: `  fleetctl markers list
  fleetctl markers list --pipeline etl
  fleetctl markers list --pipeline etl --format json`,
	RunE: runMarkersList,
}

var markersInvalidateCmd = &cobra.Command{
	Use:   "invalidate",
	Short: "Remove the marker for (pipeline, step), forcing the next verify to be stale",
	Long: `invalidate removes the marker for (pipeline, step). Idempotent:
exits 0 even when the marker did not exist.`,
	Example: `  fleetctl markers invalidate --pipeline etl --step load_users`,
	RunE:    runMarkersInvalidate,
}

func init() {
	markersCmd.PersistentFlags().StringVar(&markersDir, "markers-dir", "~/.hermes/markers", "marker storage root")

	markersHashCmd.Flags().StringVar(&markersPipeline, "pipeline", "", "pipeline name (required)")
	markersHashCmd.Flags().StringVar(&markersStep, "step", "", "step name (required)")
	markersHashCmd.Flags().StringVar(&markersInput, "input", "", "input file path, or '-' for stdin (required)")
	markersHashCmd.Flags().StringVar(&markersOutput, "output", "", "optional output path recorded on the marker")
	_ = markersHashCmd.MarkFlagRequired("pipeline")
	_ = markersHashCmd.MarkFlagRequired("step")
	_ = markersHashCmd.MarkFlagRequired("input")

	markersVerifyCmd.Flags().StringVar(&markersPipeline, "pipeline", "", "pipeline name (required)")
	markersVerifyCmd.Flags().StringVar(&markersStep, "step", "", "step name (required)")
	markersVerifyCmd.Flags().StringVar(&markersInput, "input", "", "input file path, or '-' for stdin (required)")
	_ = markersVerifyCmd.MarkFlagRequired("pipeline")
	_ = markersVerifyCmd.MarkFlagRequired("step")
	_ = markersVerifyCmd.MarkFlagRequired("input")

	markersListCmd.Flags().StringVar(&markersPipeline, "pipeline", "", "limit to one pipeline (default: all)")
	markersListCmd.Flags().StringVar(&markersFormat, "format", "table", "output format: table or json")

	markersInvalidateCmd.Flags().StringVar(&markersPipeline, "pipeline", "", "pipeline name (required)")
	markersInvalidateCmd.Flags().StringVar(&markersStep, "step", "", "step name (required)")
	_ = markersInvalidateCmd.MarkFlagRequired("pipeline")
	_ = markersInvalidateCmd.MarkFlagRequired("step")

	markersCmd.AddCommand(markersHashCmd, markersVerifyCmd, markersListCmd, markersInvalidateCmd)
	rootCmd.AddCommand(markersCmd)
}

// openMarkersStore resolves --markers-dir and opens (creating if needed)
// the on-disk store. Centralised so every subcommand reports the same
// error shape.
func openMarkersStore() (*markers.Store, error) {
	dir, err := expandHomePath(markersDir)
	if err != nil {
		return nil, fmt.Errorf("markers: expand dir: %w", err)
	}
	store, err := markers.NewStore(dir)
	if err != nil {
		return nil, fmt.Errorf("markers: open store: %w", err)
	}
	return store, nil
}

// readInputForHash hashes the input source. "-" means stdin (read from the
// cobra command so tests can inject); anything else is a file path.
func readInputForHash(cmd *cobra.Command, src string) (string, error) {
	if src == "-" {
		return markers.HashInput(io.Reader(cmd.InOrStdin()))
	}
	return markers.HashFile(src)
}

func runMarkersHash(cmd *cobra.Command, _ []string) error {
	store, err := openMarkersStore()
	if err != nil {
		return err
	}
	hash, err := readInputForHash(cmd, markersInput)
	if err != nil {
		return fmt.Errorf("markers hash: %w", err)
	}
	m := markers.Marker{
		Pipeline:   markersPipeline,
		Step:       markersStep,
		InputHash:  hash,
		OutputPath: markersOutput,
	}
	if err := store.Write(m); err != nil {
		return fmt.Errorf("markers hash: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s\n", hash)
	return nil
}

func runMarkersVerify(cmd *cobra.Command, _ []string) error {
	store, err := openMarkersStore()
	if err != nil {
		return err
	}
	hash, err := readInputForHash(cmd, markersInput)
	if err != nil {
		return fmt.Errorf("markers verify: %w", err)
	}
	fresh, err := store.IsFresh(markersPipeline, markersStep, hash)
	if err != nil {
		return fmt.Errorf("markers verify: %w", err)
	}
	if !fresh {
		// Exit code 1 is the contract that lets shell pipelines
		// `if fleetctl markers verify ...; then skip; else run; fi`.
		// We surface the stale state as an error so main() prints it
		// to stderr and returns 1.
		return fmt.Errorf("markers verify: stale: %s/%s", markersPipeline, markersStep)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "fresh: %s/%s\n", markersPipeline, markersStep)
	return nil
}

func runMarkersList(cmd *cobra.Command, _ []string) error {
	store, err := openMarkersStore()
	if err != nil {
		return err
	}

	var all []*markers.Marker
	if markersPipeline != "" {
		ms, err := store.List(markersPipeline)
		if err != nil {
			return fmt.Errorf("markers list: %w", err)
		}
		all = ms
	} else {
		pipes, err := store.Pipelines()
		if err != nil {
			return fmt.Errorf("markers list: %w", err)
		}
		for _, p := range pipes {
			ms, err := store.List(p)
			if err != nil {
				return fmt.Errorf("markers list %s: %w", p, err)
			}
			all = append(all, ms...)
		}
	}

	switch strings.ToLower(markersFormat) {
	case "json":
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		// Always emit a JSON array, never `null`, so consumers can pipe
		// through `jq .[] | ...` without special-casing the empty fleet.
		if all == nil {
			all = []*markers.Marker{}
		}
		return enc.Encode(all)
	case "table", "":
		renderMarkersTable(cmd, all)
		return nil
	default:
		return fmt.Errorf("markers list: unknown --format %q (want table|json)", markersFormat)
	}
}

func renderMarkersTable(cmd *cobra.Command, all []*markers.Marker) {
	w := cmd.OutOrStdout()
	if len(all) == 0 {
		fmt.Fprintln(w, "(no markers)")
		return
	}
	fmt.Fprintf(w, "%-20s %-24s %-24s %s\n", "Pipeline", "Step", "Completed", "InputHash")
	fmt.Fprintln(w, strings.Repeat("-", 100))
	for _, m := range all {
		ts := m.CompletedAt.Format("2006-01-02T15:04:05Z")
		hash := m.InputHash
		if len(hash) > 24 {
			hash = hash[:24] + "..."
		}
		fmt.Fprintf(w, "%-20s %-24s %-24s %s\n", m.Pipeline, m.Step, ts, hash)
	}
	fmt.Fprintf(w, "\n%d marker(s)\n", len(all))
}

func runMarkersInvalidate(cmd *cobra.Command, _ []string) error {
	store, err := openMarkersStore()
	if err != nil {
		return err
	}
	if err := store.Invalidate(markersPipeline, markersStep); err != nil {
		return fmt.Errorf("markers invalidate: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "invalidated: %s/%s\n", markersPipeline, markersStep)
	return nil
}

func markersSkill() Skill {
	return Skill{
		Name:    "markers",
		Trigger: "/markers",
		Summary: "Record and verify content-hashed step markers for idempotent pipelines (ETL-style step skip).",
		Args: []SkillArg{
			{Name: "hash", Description: "Hash --input and write a marker for --pipeline/--step."},
			{Name: "verify", Description: "Check the marker for --pipeline/--step against --input; exit 0 fresh, 1 stale/missing."},
			{Name: "list", Description: "List markers, optionally filtered by --pipeline. Use --format=json for machine output."},
			{Name: "invalidate", Description: "Remove the marker for --pipeline/--step (idempotent)."},
			{Name: "--pipeline NAME", Description: "Pipeline identifier (required for hash/verify/invalidate; optional filter for list)."},
			{Name: "--step NAME", Description: "Step identifier within the pipeline (required for hash/verify/invalidate)."},
			{Name: "--input PATH", Description: "Input file to hash, or '-' for stdin (required for hash/verify)."},
			{Name: "--output PATH", Description: "Optional output path recorded on the marker (hash only)."},
			{Name: "--format FMT", Description: "List output format: table (default) or json."},
			{Name: "--markers-dir PATH", Description: "Override the marker storage root (default: ~/.hermes/markers)."},
		},
		Examples: []SkillExample{
			{Description: "Record a marker after a step succeeds", Command: "fleetctl markers hash --pipeline etl --step load_users --input data.csv"},
			{Description: "Skip a step if its input is unchanged", Command: "fleetctl markers verify --pipeline etl --step load_users --input data.csv"},
			{Description: "List markers as JSON for a pipeline", Command: "fleetctl markers list --pipeline etl --format json"},
			{Description: "Force a step to re-run on the next pass", Command: "fleetctl markers invalidate --pipeline etl --step load_users"},
		},
	}
}

func init() { registerSkill("markers", markersSkill) }
