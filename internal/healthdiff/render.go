// render.go renders a Report into the two operator-facing formats:
// markdown (default) and JSON.
//
// The markdown renderer mirrors the bash port's per-host bullet list so
// existing operator dashboards that diff snapshot reports keep working.
// The JSON renderer is new; it's stable enough that downstream tools
// (dashboards, audit ingest, future fleetctl subcommands) can rely on
// the schema declared in the Change/Report types.

package healthdiff

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// RenderMarkdown produces the operator-facing markdown report.
//
// Output shape matches the bash port:
//
//	# Fleet health snapshot diff
//
//	- From: <basename(from)>  _(<brief>)_
//	- To:   <basename(to)>    _(<brief>)_
//	- Total change records: N
//
//	## <host>
//
//	- [<group>] <field>: `<old>` → `<new>`
//	...
//
// When totalChanges is 0, an italic "_No changes between the two
// snapshots._" line is emitted instead of host sections.
func RenderMarkdown(r Report) string {
	var b strings.Builder

	fromBrief := briefForFilename(r.FromName)
	toBrief := briefForFilename(r.ToName)

	b.WriteString("# Fleet health snapshot diff\n\n")
	fmt.Fprintf(&b, "- From: %s  _(%s)_\n", r.FromName, fromBrief)
	fmt.Fprintf(&b, "- To:   %s  _(%s)_\n", r.ToName, toBrief)
	fmt.Fprintf(&b, "- Total change records: %d\n\n", r.TotalChanges)

	if r.TotalChanges == 0 {
		b.WriteString("_No changes between the two snapshots._\n")
		return b.String()
	}

	hosts := uniqueHostsInOrder(r.Changes)
	for _, h := range hosts {
		fmt.Fprintf(&b, "## %s\n\n", h)
		var hostAdded, hostRemoved bool
		for _, c := range r.Changes {
			if c.Host != h {
				continue
			}
			if c.Kind == KindHostAdded {
				hostAdded = true
			}
			if c.Kind == KindHostRemoved {
				hostRemoved = true
			}
		}
		if hostAdded {
			b.WriteString("- **NEW HOST** — not present in the previous snapshot.\n")
		}
		if hostRemoved {
			b.WriteString("- **REMOVED HOST** — not present in the latest snapshot.\n")
		}
		for _, c := range r.Changes {
			if c.Host != h {
				continue
			}
			switch c.Kind {
			case KindChanged:
				fmt.Fprintf(&b, "- [%s] %s: `%s` → `%s`\n",
					FieldGroup(c.Field), c.Field, c.Old, c.New)
			case KindAdded:
				fmt.Fprintf(&b, "- [%s] %s: (was unset) → `%s`\n",
					FieldGroup(c.Field), c.Field, c.New)
			case KindRemoved:
				fmt.Fprintf(&b, "- [%s] %s: `%s` → (no longer reported)\n",
					FieldGroup(c.Field), c.Field, c.Old)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// RenderJSON marshals the report as compact JSON. Returns the bytes and
// any encoding error from the stdlib encoder (which in practice never
// fires for the Report schema, but the contract preserves it).
//
// The output is single-line JSON terminated with a newline so the CLI
// can stream it to stdout and downstream `jq` pipelines see one record
// per invocation, matching the bash port's contract.
func RenderJSON(r Report) ([]byte, error) {
	if r.GeneratedAt == "" {
		r.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	}
	out, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("healthdiff: marshal: %w", err)
	}
	return append(out, '\n'), nil
}

// uniqueHostsInOrder returns the host names in their first-seen order.
// Diff() already sorts Changes by Host then Field then Kind, so this
// preserves a stable alphabetical traversal.
func uniqueHostsInOrder(changes []Change) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, c := range changes {
		if _, ok := seen[c.Host]; ok {
			continue
		}
		seen[c.Host] = struct{}{}
		out = append(out, c.Host)
	}
	return out
}

// briefForFilename humanises a fleet-snapshot filename for the markdown
// header: "fleet-snapshot-20260515T220009Z.md" → "2026-05-15 22:00:09Z".
// Files that don't match the pattern get the basename echoed back —
// keeps the renderer permissive on caller-supplied labels.
var snapshotFilenameRe = regexp.MustCompile(`^fleet-snapshot-([0-9]{8}T[0-9]{6}Z)\.md$`)

func briefForFilename(name string) string {
	base := name
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	m := snapshotFilenameRe.FindStringSubmatch(base)
	if m == nil {
		return base
	}
	ts := m[1] // e.g. 20260515T220009Z
	return fmt.Sprintf("%s-%s-%s %s:%s:%sZ",
		ts[0:4], ts[4:6], ts[6:8],
		ts[9:11], ts[11:13], ts[13:15])
}
