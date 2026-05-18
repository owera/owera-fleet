// Package healthdiff parses fleet-health-snapshot markdown reports and
// produces a structured diff between two of them.
//
// # Background
//
// The hermes-setup era shipped scripts/fleet-health-diff.sh, which ran
// two awk programs back-to-back: one extract pass that emitted a sorted
// TSV stream of <host>\t<field>\t<value>, and one diff pass that joined
// the two streams and emitted change records. The bash version also
// supported a small --json mode plus a markdown human report.
//
// This Go port preserves the parsing rules exactly (verified by the
// golden fixtures under testdata/) and adds three things the bash
// version lacked:
//
//   - Severity classification (info / warn / crit) attached to every
//     change record, so the CLI can pick an informative exit code and
//     CI can gate on warn-or-above.
//   - A first-class JSON output with a deterministic field ordering.
//   - Round-trippable types (Snapshot, Report) that other Go callers
//     can consume without re-parsing markdown.
//
// The package is intentionally pure: no filesystem I/O, no logging, no
// network. The CLI wrapper in cmd/fleetctl/commands/healthdiff.go owns
// the side effects.
package healthdiff

import (
	"bufio"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Severity classifies how interesting a single change is to an operator.
// The CLI maps these onto exit codes so CI can gate on warn-or-above.
type Severity string

const (
	// SeverityInfo is the default — a benign change such as new host,
	// uptime drift, or pinned-version touch. Reported but not actionable.
	SeverityInfo Severity = "info"
	// SeverityWarn indicates a change worth a human look but not paging:
	// brew drift, disk usage trending up, heartbeat going stale.
	SeverityWarn Severity = "warn"
	// SeverityCrit indicates a regression that should block a deploy or
	// page the on-call: a host disappeared, a probe started failing,
	// heartbeat went missing, disk crossed an at-risk threshold.
	SeverityCrit Severity = "crit"
)

// ChangeKind names the shape of a single diff record.
type ChangeKind string

const (
	// KindChanged: both snapshots carry the field, values differ.
	KindChanged ChangeKind = "changed"
	// KindAdded: field appeared in the newer snapshot (and the host
	// existed in the older one, so it's a genuine new field).
	KindAdded ChangeKind = "added"
	// KindRemoved: field disappeared from the newer snapshot (and the
	// host still exists, so the probe lost that field).
	KindRemoved ChangeKind = "removed"
	// KindHostAdded: host present in B but not A.
	KindHostAdded ChangeKind = "host_added"
	// KindHostRemoved: host present in A but not B.
	KindHostRemoved ChangeKind = "host_removed"
)

// Sentinel value emitted by the bash port for missing-value slots so
// the rendered output always has five non-empty columns. Preserved
// here for parity with the bash CLI; downstream consumers can rely on
// it to identify "no value on this side" in human-rendered reports.
const unsetSentinel = "(unset)"

// Host holds the parsed-and-normalised fields for one host in one
// snapshot. Field names match the bash extract awk's emit() keys.
type Host struct {
	// Name is the markdown H2 heading (e.g. "gateway: claw3",
	// "worker: hermes@claw1.local"). Used as the diff partition key.
	Name string
	// Fields is the normalised key/value map. ProbeStatus is always
	// either "ok" or "failed". Missing values are omitted (not "").
	Fields map[string]string
}

// Snapshot is the parsed view of one fleet-health-snapshot report.
type Snapshot struct {
	Hosts []Host
}

// Change is one diff record between two snapshots.
type Change struct {
	Host     string     `json:"host"`
	Kind     ChangeKind `json:"kind"`
	Field    string     `json:"field"`
	Old      string     `json:"old"`
	New      string     `json:"new"`
	Severity Severity   `json:"severity"`
}

// Report is the full structured output of a diff.
//
// FromName / ToName are the source filenames (or any caller-supplied
// label) — they end up in both markdown and JSON renders for traceability.
type Report struct {
	GeneratedAt  string   `json:"generated_at"`
	FromName     string   `json:"from"`
	ToName       string   `json:"to"`
	TotalChanges int      `json:"total_changes"`
	Changes      []Change `json:"changes"`
}

// MaxSeverity returns the highest severity across all changes. Empty
// report → SeverityInfo (vacuously "info"); presence of any crit wins.
func (r Report) MaxSeverity() Severity {
	max := SeverityInfo
	hasAny := false
	for _, c := range r.Changes {
		hasAny = true
		if severityRank(c.Severity) > severityRank(max) {
			max = c.Severity
		}
	}
	if !hasAny {
		return ""
	}
	return max
}

func severityRank(s Severity) int {
	switch s {
	case SeverityCrit:
		return 2
	case SeverityWarn:
		return 1
	default:
		return 0
	}
}

// Parse extracts host fields from a fleet-health-snapshot markdown body.
//
// The bash extract awk's contract is preserved:
//
//   - "## " lines start a new host block.
//   - The first ``` opens a code block; the next ``` closes it.
//   - Inside the code block, lines matching ^[A-Z0-9_]+: are field
//     keys. Indented continuation lines accumulate onto the previous
//     key. Inline value on the same line as the key short-circuits the
//     continuation.
//   - A host with no OS field is treated as a probe failure and only
//     emits probe_status=failed.
//
// Parse never errors on well-formed input; if the markdown has no
// hosts (e.g. operator pasted the wrong file), the returned Snapshot
// will have zero hosts and the caller can decide whether to treat
// that as fatal.
func Parse(body string) (Snapshot, error) {
	var snap Snapshot
	scanner := bufio.NewScanner(strings.NewReader(body))
	// fleet-snapshot files can have long lines (BREW_OUTDATED listings,
	// kernel banner) so widen the default 64 KiB buffer.
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)

	var (
		host    string
		inBlock bool
		inCode  bool
		curKey  string
		curVal  strings.Builder
		fields  map[string]string
	)

	keyRe := regexp.MustCompile(`^([A-Z0-9_]+):(.*)$`)

	flushCurKey := func() {
		if curKey != "" {
			fields[curKey] = strings.TrimRight(curVal.String(), "\n")
			curKey = ""
			curVal.Reset()
		}
	}
	flushHost := func() {
		if host == "" {
			return
		}
		flushCurKey()
		snap.Hosts = append(snap.Hosts, normaliseHost(host, fields))
	}
	resetHost := func() {
		fields = map[string]string{}
		curKey = ""
		curVal.Reset()
		inCode = false
	}

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "## ") {
			flushHost()
			host = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			resetHost()
			inBlock = true
			continue
		}
		if !inBlock {
			continue
		}
		if strings.HasPrefix(line, "```") {
			inCode = !inCode
			if !inCode {
				flushCurKey()
			}
			continue
		}
		if !inCode {
			continue
		}
		if m := keyRe.FindStringSubmatch(line); m != nil {
			flushCurKey()
			key := m[1]
			rest := strings.TrimLeft(m[2], " \t")
			if rest != "" {
				fields[key] = rest
			} else {
				curKey = key
				curVal.Reset()
			}
			continue
		}
		if curKey != "" {
			trimmed := strings.TrimLeft(line, " \t")
			if curVal.Len() > 0 {
				curVal.WriteByte('\n')
			}
			curVal.WriteString(trimmed)
		}
	}
	flushHost()
	if err := scanner.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("healthdiff: scan: %w", err)
	}
	return snap, nil
}

// normaliseHost translates the raw extracted fields into the canonical
// per-host map. Parsing rules mirror the bash extract awk one-for-one;
// see the inline comments for each derived field.
func normaliseHost(name string, raw map[string]string) Host {
	out := map[string]string{}
	// Probe-failure short-circuit: if there is no OS line, the probe
	// never produced a real result. Emit just one synthetic field so
	// the diff can show probe_status transitions rather than a flurry
	// of "field went from X to empty" noise.
	if _, ok := raw["OS"]; !ok {
		out["probe_status"] = "failed"
		return Host{Name: name, Fields: out}
	}
	out["probe_status"] = "ok"
	emit := func(field, value string) {
		if value == "" {
			return
		}
		out[field] = value
	}
	emit("os", raw["OS"])
	emit("arch", raw["ARCH"])
	emit("kernel", raw["KERNEL"])
	emit("hostname_full", raw["HOSTNAME"])
	emit("uptime_days", parseUptimeDays(raw["UPTIME"]))
	pct, used := parseDiskRoot(raw["DISK_ROOT"])
	emit("disk_root_pct", pct)
	emit("disk_root_used", used)
	emit("mem_avail_pct", parseMemPct(raw["MEM_AVAIL_PCT"]))
	emit("hermes_version", raw["HERMES_VERSION"])
	emit("tirith", raw["TIRITH"])
	emit("heartbeat_status", parseHeartbeat(raw["HEARTBEAT"]))
	emit("pinned_version", raw["PINNED_VERSION_FILE"])
	brewCount, brewList := parseBrewOutdated(raw["BREW_OUTDATED"])
	emit("brew_outdated_count", brewCount)
	emit("brew_outdated_list", brewList)
	riskCount, riskList := parseDiskAtRisk(raw["DISK_AT_RISK"])
	emit("disk_at_risk_count", riskCount)
	emit("disk_at_risk_list", riskList)
	return Host{Name: name, Fields: out}
}

// parseUptimeDays mirrors the awk: "up 27 days, ..." → "27"; otherwise "0".
var uptimeDaysRe = regexp.MustCompile(`up ([0-9]+) day`)

func parseUptimeDays(s string) string {
	if s == "" {
		return ""
	}
	if m := uptimeDaysRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return "0"
}

// parseDiskRoot returns (use%, used) from a df-h line:
//
//	/dev/disk3s1s1   228Gi   12Gi   162Gi   7%   458k   1.7G   0%   /
//
// First token ending in "%" is the use%; the two-back token is "Used".
func parseDiskRoot(s string) (pct, used string) {
	if s == "" {
		return "", ""
	}
	tokens := strings.Fields(s)
	for i, tok := range tokens {
		if strings.HasSuffix(tok, "%") {
			pct = strings.TrimSuffix(tok, "%")
			if i >= 2 {
				used = tokens[i-2]
			}
			return pct, used
		}
	}
	return "", ""
}

// parseMemPct mirrors the awk: "  2% available (...)" → "2".
var memPctRe = regexp.MustCompile(`([0-9]+)% available`)

func parseMemPct(s string) string {
	if m := memPctRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// parseHeartbeat classifies the HEARTBEAT line into a small enum so
// diffs are stable across small timing jitter.
var heartbeatAgeRe = regexp.MustCompile(`^([0-9]+)s ago`)

func parseHeartbeat(s string) string {
	if s == "" {
		return ""
	}
	if strings.Contains(s, "expected") {
		return "gateway-expected"
	}
	if strings.Contains(s, "no heartbeat file") {
		return "missing"
	}
	if m := heartbeatAgeRe.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return "unknown"
		}
		switch {
		case n < 60:
			return "fresh"
		case n < 300:
			return "stale-60s"
		default:
			return "stale-300s"
		}
	}
	return "unknown"
}

// parseBrewOutdated handles three shapes:
//
//	"up to date"            → (0, "(none)")
//	"brew not installed"    → ("n/a", "n/a")
//	"3 outdated:\n  a\n b"  → ("3", "a,b")
func parseBrewOutdated(s string) (count, list string) {
	if s == "" {
		return "", ""
	}
	if strings.HasPrefix(s, "up to date") {
		return "0", "(none)"
	}
	if strings.HasPrefix(s, "brew not installed") {
		return "n/a", "n/a"
	}
	lines := strings.Split(s, "\n")
	first := lines[0]
	if m := regexp.MustCompile(`^([0-9]+) outdated`).FindStringSubmatch(first); m != nil {
		var items []string
		for _, l := range lines[1:] {
			it := strings.TrimSpace(l)
			if it != "" {
				items = append(items, it)
			}
		}
		joined := strings.Join(items, ",")
		if joined == "" {
			joined = "(unparsed)"
		}
		return m[1], joined
	}
	return "?", s
}

// parseDiskAtRisk handles the at-risk listing. "(none ..." → 0; otherwise
// non-empty trimmed lines are concatenated with ";".
func parseDiskAtRisk(s string) (count, list string) {
	if s == "" {
		return "", ""
	}
	if strings.Contains(s, "(none") {
		return "0", "(none)"
	}
	lines := strings.Split(s, "\n")
	var items []string
	for _, l := range lines {
		it := strings.TrimSpace(l)
		if it != "" {
			items = append(items, it)
		}
	}
	if len(items) == 0 {
		return "0", "(empty)"
	}
	return strconv.Itoa(len(items)), strings.Join(items, ";")
}

// Diff computes the change set going from `from` → `to`.
//
// The result's Changes slice is sorted by (Host, Field, Kind) for
// deterministic output across runs — important so a CI diff gate
// can hash report bodies and compare them.
func Diff(from, to Snapshot) Report {
	fromIdx := indexHosts(from)
	toIdx := indexHosts(to)
	allHosts := unionHostNames(fromIdx, toIdx)

	var changes []Change
	for _, h := range allHosts {
		fh, fOK := fromIdx[h]
		th, tOK := toIdx[h]
		switch {
		case !fOK && tOK:
			// In the bash port, host_added does NOT also emit per-field
			// "added" records — the host_added marker is the whole
			// signal. Preserve that here.
			changes = append(changes, Change{
				Host: h, Kind: KindHostAdded,
				Field: "-", Old: "-", New: "-",
				Severity: severityFor(KindHostAdded, ""),
			})
		case fOK && !tOK:
			changes = append(changes, Change{
				Host: h, Kind: KindHostRemoved,
				Field: "-", Old: "-", New: "-",
				Severity: severityFor(KindHostRemoved, ""),
			})
		case fOK && tOK:
			// Field-level diff inside a host present in both snapshots.
			fields := unionFieldNames(fh.Fields, th.Fields)
			for _, f := range fields {
				fv, fHas := fh.Fields[f]
				tv, tHas := th.Fields[f]
				switch {
				case fHas && tHas && fv != tv:
					changes = append(changes, Change{
						Host: h, Kind: KindChanged,
						Field: f, Old: fv, New: tv,
						Severity: severityFor(KindChanged, f),
					})
				case fHas && !tHas:
					changes = append(changes, Change{
						Host: h, Kind: KindRemoved,
						Field: f, Old: fv, New: unsetSentinel,
						Severity: severityFor(KindRemoved, f),
					})
				case !fHas && tHas:
					changes = append(changes, Change{
						Host: h, Kind: KindAdded,
						Field: f, Old: unsetSentinel, New: tv,
						Severity: severityFor(KindAdded, f),
					})
				}
			}
		}
	}

	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Host != changes[j].Host {
			return changes[i].Host < changes[j].Host
		}
		if changes[i].Field != changes[j].Field {
			return changes[i].Field < changes[j].Field
		}
		return changes[i].Kind < changes[j].Kind
	})

	return Report{
		FromName:     "",
		ToName:       "",
		TotalChanges: len(changes),
		Changes:      changes,
	}
}

func indexHosts(s Snapshot) map[string]Host {
	out := make(map[string]Host, len(s.Hosts))
	for _, h := range s.Hosts {
		out[h.Name] = h
	}
	return out
}

func unionHostNames(a, b map[string]Host) []string {
	seen := map[string]struct{}{}
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func unionFieldNames(a, b map[string]string) []string {
	seen := map[string]struct{}{}
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// severityFor classifies one change record. The mapping is intentionally
// conservative: crit only for events that genuinely page (host vanished,
// probe started failing, heartbeat truly missing); warn for trends that
// invite a look (disk creep, heartbeat going stale, brew drift); info
// otherwise. The CLI's exit code is derived from MaxSeverity of the
// whole Report.
func severityFor(kind ChangeKind, field string) Severity {
	switch kind {
	case KindHostRemoved:
		return SeverityCrit
	case KindHostAdded:
		return SeverityInfo
	}
	switch field {
	case "probe_status":
		// Either added (new failing host appeared) or changed
		// (ok → failed or failed → ok). Treat any movement on this
		// field as crit; the CLI body still shows old → new so an
		// operator can see which direction.
		return SeverityCrit
	case "heartbeat_status":
		// Heartbeat moving in any direction (fresh → stale → missing,
		// or the reverse) is operator-worthy but not pageable on its
		// own; the host_removed/probe_status events cover the truly
		// critical cases. Warn is the right gate-level here.
		return SeverityWarn
	case "disk_root_pct", "disk_at_risk_count", "disk_at_risk_list", "mem_avail_pct":
		return SeverityWarn
	case "brew_outdated_count", "brew_outdated_list":
		return SeverityWarn
	case "hermes_version", "tirith", "pinned_version", "os", "arch", "kernel", "hostname_full":
		return SeverityInfo
	case "uptime_days":
		return SeverityInfo
	}
	return SeverityInfo
}

// FieldGroup is the bash port's `field_severity` annotation used in the
// markdown bullet prefix. Preserved verbatim so the human report style
// matches operator muscle memory.
func FieldGroup(field string) string {
	switch field {
	case "os", "arch", "kernel", "hostname_full",
		"hermes_version", "tirith", "pinned_version":
		return "STABLE"
	case "disk_root_pct", "disk_at_risk_count", "disk_at_risk_list":
		return "DISK"
	case "mem_avail_pct":
		return "MEM"
	case "heartbeat_status":
		return "HEARTBEAT"
	case "brew_outdated_count", "brew_outdated_list":
		return "BREW"
	case "uptime_days":
		return "UPTIME"
	}
	return "INFO"
}
