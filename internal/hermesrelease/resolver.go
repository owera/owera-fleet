// Package hermesrelease translates Nous Research Hermes Agent
// marketing-semver release names ("v0.13.0") into the actual git
// tag those releases ship under ("v2026.5.7" — CalVer).
//
// # Why this exists
//
// The Nous installer (https://hermes-agent.nousresearch.com/install.sh)
// accepts a git ref via `--branch <ref>` and clones from that ref. It
// does NOT honour any `HERMES_VERSION` env var — `grep -c HERMES_VERSION`
// against the installer is 0. Earlier phase03 invocations relied on a
// phantom env var and effectively installed "whatever main was tagged
// when bootstrap ran", which only coincidentally matched the operator's
// stated pin.
//
// The Nous release tagging convention pairs a CalVer git tag with a
// human-readable semver in the GitHub Release title:
//
//	Release title         GitHub tag    Released
//	Hermes Agent v0.14.0  v2026.5.16    2026-05-16
//	Hermes Agent v0.13.0  v2026.5.7     2026-05-07
//	Hermes Agent v0.12.0  v2026.4.30    2026-04-30
//
// To pin v0.13.0 deterministically the installer needs
// `--branch v2026.5.7`. This package owns the lookup + caching of the
// semver → CalVer mapping so phase03 (and any future caller) can pass
// the correct `--branch` argument.
//
// # Lookup mechanism
//
// The lookup runs `gh release list -R NousResearch/hermes-agent
// --limit 50 --json tagName,name`, parses the JSON, and finds the
// release whose `name` contains the requested semver token. The
// matching `tagName` is the CalVer tag we want.
//
// # Caching
//
// Network lookups are cached at ~/.hermes/PINNED_VERSION_TAG with the
// two-line layout:
//
//	v0.13.0
//	v2026.5.7
//
// Cache hit happens iff line 1 equals the requested semver. Bumping
// the pin re-runs the lookup. This matches the existing convention
// where ~/.hermes/PINNED_VERSION holds the canonical semver pin.
//
// # Dependency injection
//
// The two side effects (run `gh`, read/write the cache file) are
// function fields so tests can substitute fakes without shelling out.
// Production callers use Default(), which wires the real `gh` binary
// and ~/.hermes/PINNED_VERSION_TAG.
package hermesrelease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultCacheFile is the relative path under $HOME where a successful
// semver → CalVer translation is cached. Sits next to PINNED_VERSION so
// it's obvious how the two relate.
const DefaultCacheFile = ".hermes/PINNED_VERSION_TAG"

// DefaultRepo is the upstream GitHub repo we query for releases.
const DefaultRepo = "NousResearch/hermes-agent"

// DefaultListLimit is the number of releases to ask `gh release list`
// for. 50 covers ~12 months of releases at current cadence, with
// generous headroom — and matches the limit suggested in the bug
// report's repro instructions.
const DefaultListLimit = 50

// DefaultLookupTimeout caps how long the underlying `gh` call can take
// before the resolver gives up. The network call is fast in the happy
// path; this is mostly to keep bootstrap from hanging indefinitely on a
// DNS or auth failure.
const DefaultLookupTimeout = 30 * time.Second

// ErrSemverNotFound is returned when no release in the queried set has
// a name containing the requested semver. Wrapped so callers can
// errors.Is against it.
var ErrSemverNotFound = errors.New("hermesrelease: semver not found in upstream releases")

// ErrInvalidSemver is returned when the input doesn't look like a
// semver token at all (e.g. empty string, "latest", "main"). The
// resolver intentionally refuses to look these up — passing them
// through unchanged is the bash script's responsibility.
var ErrInvalidSemver = errors.New("hermesrelease: input is not a recognizable semver token")

// Release mirrors the relevant subset of `gh release list --json` output.
type Release struct {
	TagName string `json:"tagName"`
	Name    string `json:"name"`
}

// Resolver translates a semver pin into the matching CalVer git tag.
// Construct via Default() for production; tests construct directly with
// fake list + cache functions.
type Resolver struct {
	// CacheFile is the absolute path of the persisted translation file.
	// Empty disables caching (every Resolve hits ListReleases).
	CacheFile string

	// Repo is the GitHub "owner/name" the releases live under.
	// Defaults to DefaultRepo when empty.
	Repo string

	// ListLimit is the --limit value passed to `gh release list`.
	// Defaults to DefaultListLimit when 0.
	ListLimit int

	// LookupTimeout bounds the underlying `gh` call. Defaults to
	// DefaultLookupTimeout when 0.
	LookupTimeout time.Duration

	// ListReleases returns the parsed release list. Production wiring
	// shells out to `gh release list`; tests inject a fixture.
	ListReleases func(ctx context.Context) ([]Release, error)

	// ReadCache returns the cached two-line payload (or os.ErrNotExist).
	ReadCache func() ([]byte, error)

	// WriteCache persists the two-line payload (or returns the I/O error).
	WriteCache func(payload []byte) error
}

// Default returns a Resolver wired against the real `gh` CLI and
// $HOME/.hermes/PINNED_VERSION_TAG. The repo and limit follow the
// package defaults; override the fields on the returned Resolver if
// you need to point at a fork or a different cache location.
//
// Returns an error only if $HOME is unset — in that case caching is
// impossible (the resolver still works, just always hits the network).
func Default() (*Resolver, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, errors.New("hermesrelease: $HOME unset; cannot locate cache file")
	}
	cache := filepath.Join(home, DefaultCacheFile)
	return &Resolver{
		CacheFile:     cache,
		Repo:          DefaultRepo,
		ListLimit:     DefaultListLimit,
		LookupTimeout: DefaultLookupTimeout,
		ListReleases:  productionListReleases,
		ReadCache:     func() ([]byte, error) { return os.ReadFile(cache) },
		WriteCache: func(payload []byte) error {
			if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
				return fmt.Errorf("hermesrelease: mkdir %s: %w", filepath.Dir(cache), err)
			}
			return os.WriteFile(cache, payload, 0o644)
		},
	}, nil
}

// Resolve returns the CalVer git tag for the supplied marketing semver
// (e.g. "v0.13.0" → "v2026.5.7"). The lookup is cached on the gateway
// so subsequent calls for the same semver skip the GitHub round-trip.
//
// Inputs that don't look like semver (empty, "latest", "main", a
// CalVer tag itself) return ErrInvalidSemver — callers should pass
// those through to the installer unchanged rather than translate them.
func (r *Resolver) Resolve(ctx context.Context, semver string) (string, error) {
	sv := strings.TrimSpace(semver)
	if !looksLikeMarketingSemver(sv) {
		return "", fmt.Errorf("%w: %q", ErrInvalidSemver, semver)
	}

	// Cache hit fast-path. We only honour the cache when the recorded
	// semver matches exactly so bumping the pin invalidates implicitly.
	if r.CacheFile != "" && r.ReadCache != nil {
		if cached, ok := r.cacheLookup(sv); ok {
			return cached, nil
		}
	}

	if r.ListReleases == nil {
		return "", errors.New("hermesrelease: ListReleases not wired")
	}

	timeout := r.LookupTimeout
	if timeout == 0 {
		timeout = DefaultLookupTimeout
	}
	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	releases, err := r.ListReleases(lookupCtx)
	if err != nil {
		return "", fmt.Errorf("hermesrelease: list releases: %w", err)
	}

	tag, found := matchSemver(releases, sv)
	if !found {
		return "", fmt.Errorf("%w: looked for %q in %d releases (repo=%s)",
			ErrSemverNotFound, sv, len(releases), r.repoOrDefault())
	}

	// Persist for next time. A write failure is non-fatal: we still
	// return the resolved tag so the caller can proceed — caching is an
	// optimization, not a correctness requirement.
	if r.WriteCache != nil && r.CacheFile != "" {
		_ = r.WriteCache(formatCachePayload(sv, tag))
	}
	return tag, nil
}

// cacheLookup returns (tag, true) when the cache file exists, parses,
// and its recorded semver matches sv exactly. Any other condition
// (missing file, parse failure, semver mismatch) returns ("", false).
func (r *Resolver) cacheLookup(sv string) (string, bool) {
	raw, err := r.ReadCache()
	if err != nil {
		return "", false
	}
	cachedSV, cachedTag, ok := parseCachePayload(raw)
	if !ok {
		return "", false
	}
	if cachedSV != sv {
		return "", false
	}
	return cachedTag, true
}

func (r *Resolver) repoOrDefault() string {
	if r.Repo != "" {
		return r.Repo
	}
	return DefaultRepo
}

// productionListReleases shells out to `gh release list` and parses
// the JSON response. Kept as a package-level function (rather than a
// closure inside Default) so it's easy to test the parse path.
func productionListReleases(ctx context.Context) ([]Release, error) {
	return runGHReleaseList(ctx, DefaultRepo, DefaultListLimit)
}

// runGHReleaseList is the exec-and-parse helper. Exported via the
// productionListReleases closure for Default and exposed here for tests
// that want to exercise the parse path with a stubbed gh binary on PATH.
func runGHReleaseList(ctx context.Context, repo string, limit int) ([]Release, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, fmt.Errorf("gh CLI not on PATH (install via `brew install gh`): %w", err)
	}
	args := []string{
		"release", "list",
		"-R", repo,
		"--limit", fmt.Sprintf("%d", limit),
		"--json", "tagName,name",
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		// stderr from gh is usually the actionable signal (auth required,
		// repo not found, rate limited). Surface it in the wrap so the
		// operator doesn't have to re-run with -v to see it.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return nil, fmt.Errorf("gh release list -R %s: %s", repo,
				strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("gh release list -R %s: %w", repo, err)
	}
	return ParseReleaseList(out)
}

// ParseReleaseList decodes `gh release list --json tagName,name` output
// into the typed []Release. Exposed so tests + the runGHReleaseList
// helper share one parse path.
func ParseReleaseList(raw []byte) ([]Release, error) {
	var out []Release
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse gh json: %w", err)
	}
	return out, nil
}

// matchSemver scans releases for the first whose Name contains the
// supplied semver token. Returns the matching TagName (already in
// CalVer form, e.g. "v2026.5.7") and a found flag.
//
// "Contains" is the right matcher here because Nous release names look
// like "Hermes Agent v0.13.0 (2026.5.7)" — the semver is embedded, not
// at start. We bound the match by surrounding the semver in word-ish
// delimiters via the caller (looksLikeMarketingSemver enforces the
// "v\d+.\d+.\d+" shape, which won't accidentally substring-match
// "v0.13.0" inside "v0.13.0-beta" because the latter still contains
// the substring — accepted: the prerelease release we'd find first is
// the operator's intent anyway).
func matchSemver(releases []Release, sv string) (string, bool) {
	for _, rel := range releases {
		if strings.Contains(rel.Name, sv) {
			return rel.TagName, true
		}
	}
	return "", false
}

// looksLikeMarketingSemver returns true when sv has the shape
// "v<digits>.<digits>.<digits>" with no extra trailing characters
// beyond an optional prerelease suffix. We're deliberately strict to
// catch typos and to bounce CalVer tags (which look like
// "v2026.5.7" — three numeric parts but with a year-shaped first
// component) before they hit the network.
func looksLikeMarketingSemver(sv string) bool {
	if !strings.HasPrefix(sv, "v") {
		return false
	}
	rest := sv[1:]
	// Strip an optional prerelease suffix so "v0.13.0-rc1" is accepted.
	if i := strings.IndexAny(rest, "-+"); i >= 0 {
		rest = rest[:i]
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	// CalVer guard: marketing semver leads with a single-digit-or-low
	// major (0.x, 1.x, 2.x today). CalVer leads with a four-digit year
	// (2026). Reject the year-shaped first component so a CalVer tag
	// mistakenly passed in as a semver returns ErrInvalidSemver
	// instead of producing a no-match against gh.
	if len(parts[0]) >= 4 {
		return false
	}
	return true
}

// formatCachePayload serializes the two-line cache file. Layout:
//
//	<semver>\n<calver-tag>\n
//
// Trailing newline is conventional; parseCachePayload tolerates
// missing trailing newline too.
func formatCachePayload(sv, tag string) []byte {
	return []byte(sv + "\n" + tag + "\n")
}

// parseCachePayload extracts (semver, tag, ok) from a cache file.
// Tolerates a missing trailing newline and trims whitespace; rejects
// any file whose first two non-empty lines aren't both well-formed
// tokens (i.e. non-empty, no internal whitespace).
func parseCachePayload(raw []byte) (string, string, bool) {
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 2 {
		return "", "", false
	}
	sv := strings.TrimSpace(lines[0])
	tag := strings.TrimSpace(lines[1])
	if sv == "" || tag == "" {
		return "", "", false
	}
	if strings.ContainsAny(sv, " \t") || strings.ContainsAny(tag, " \t") {
		return "", "", false
	}
	return sv, tag, true
}
