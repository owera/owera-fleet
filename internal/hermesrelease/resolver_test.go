package hermesrelease

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fixtureReleases mirrors the live `gh release list -R NousResearch/hermes-agent`
// output as of the bug report. Kept verbatim so the test asserts the
// exact mapping the bug report's acceptance criterion calls for.
var fixtureReleases = []Release{
	{TagName: "v2026.5.16", Name: "Hermes Agent v0.14.0 (2026.5.16)"},
	{TagName: "v2026.5.7", Name: "Hermes Agent v0.13.0 (2026.5.7)"},
	{TagName: "v2026.4.30", Name: "Hermes Agent v0.12.0 (2026.4.30)"},
	{TagName: "v2026.4.10", Name: "Hermes Agent v0.11.0 (2026.4.10)"},
}

// TestResolve_SemverToCalver covers the table the bug report calls out
// explicitly: v0.14.0 → v2026.5.16, v0.13.0 → v2026.5.7,
// v0.12.0 → v2026.4.30. Plus the "unknown semver returns
// ErrSemverNotFound" + "garbage input returns ErrInvalidSemver" paths.
func TestResolve_SemverToCalver(t *testing.T) {
	cases := []struct {
		name      string
		semver    string
		wantTag   string
		wantErrIs error
	}{
		{name: "v0.14.0", semver: "v0.14.0", wantTag: "v2026.5.16"},
		{name: "v0.13.0", semver: "v0.13.0", wantTag: "v2026.5.7"},
		{name: "v0.12.0", semver: "v0.12.0", wantTag: "v2026.4.30"},
		{name: "v0.11.0", semver: "v0.11.0", wantTag: "v2026.4.10"},
		{name: "whitespace-tolerant", semver: "  v0.13.0\n", wantTag: "v2026.5.7"},
		{name: "unknown-semver", semver: "v9.9.9", wantErrIs: ErrSemverNotFound},
		{name: "empty-input", semver: "", wantErrIs: ErrInvalidSemver},
		{name: "non-semver-token", semver: "latest", wantErrIs: ErrInvalidSemver},
		{name: "calver-rejected", semver: "v2026.5.7", wantErrIs: ErrInvalidSemver},
		{name: "missing-v-prefix", semver: "0.13.0", wantErrIs: ErrInvalidSemver},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := &Resolver{
				ListReleases: func(_ context.Context) ([]Release, error) {
					return fixtureReleases, nil
				},
			}
			got, err := r.Resolve(context.Background(), tc.semver)
			if tc.wantErrIs != nil {
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("err = %v, want errors.Is(%v)", err, tc.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantTag {
				t.Errorf("Resolve(%q) = %q, want %q", tc.semver, got, tc.wantTag)
			}
		})
	}
}

// TestResolve_CacheHit_SkipsList confirms that a cache file recording
// the same semver short-circuits the network call.
func TestResolve_CacheHit_SkipsList(t *testing.T) {
	cacheFile := filepath.Join(t.TempDir(), "PINNED_VERSION_TAG")
	if err := os.WriteFile(cacheFile, formatCachePayload("v0.13.0", "v2026.5.7"), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	var listCalls int32
	r := &Resolver{
		CacheFile: cacheFile,
		ReadCache: func() ([]byte, error) { return os.ReadFile(cacheFile) },
		WriteCache: func(p []byte) error {
			return os.WriteFile(cacheFile, p, 0o644)
		},
		ListReleases: func(_ context.Context) ([]Release, error) {
			atomic.AddInt32(&listCalls, 1)
			return fixtureReleases, nil
		},
	}

	tag, err := r.Resolve(context.Background(), "v0.13.0")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tag != "v2026.5.7" {
		t.Errorf("tag = %q, want v2026.5.7", tag)
	}
	if got := atomic.LoadInt32(&listCalls); got != 0 {
		t.Errorf("cache hit should skip ListReleases, got %d calls", got)
	}
}

// TestResolve_CacheMiss_DifferentSemver triggers a network call even
// though the cache exists, because the recorded semver doesn't match.
// After the call, the cache must record the new pair.
func TestResolve_CacheMiss_DifferentSemver(t *testing.T) {
	cacheFile := filepath.Join(t.TempDir(), "PINNED_VERSION_TAG")
	// Seed with an old pin.
	if err := os.WriteFile(cacheFile, formatCachePayload("v0.12.0", "v2026.4.30"), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	var listCalls int32
	r := &Resolver{
		CacheFile: cacheFile,
		ReadCache: func() ([]byte, error) { return os.ReadFile(cacheFile) },
		WriteCache: func(p []byte) error {
			return os.WriteFile(cacheFile, p, 0o644)
		},
		ListReleases: func(_ context.Context) ([]Release, error) {
			atomic.AddInt32(&listCalls, 1)
			return fixtureReleases, nil
		},
	}

	tag, err := r.Resolve(context.Background(), "v0.13.0")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tag != "v2026.5.7" {
		t.Errorf("tag = %q, want v2026.5.7", tag)
	}
	if got := atomic.LoadInt32(&listCalls); got != 1 {
		t.Errorf("cache miss should call ListReleases once, got %d", got)
	}

	// Verify cache rewritten with the new pair.
	updated, err := os.ReadFile(cacheFile)
	if err != nil {
		t.Fatalf("read updated cache: %v", err)
	}
	sv, calver, ok := parseCachePayload(updated)
	if !ok {
		t.Fatalf("cache reparse failed: %q", updated)
	}
	if sv != "v0.13.0" || calver != "v2026.5.7" {
		t.Errorf("cache after resolve = (%q, %q), want (v0.13.0, v2026.5.7)", sv, calver)
	}
}

// TestResolve_CacheMiss_MalformedFile falls through to the network when
// the cache file is unparseable (e.g. truncated, written by an older
// tool). The lookup result is then persisted as a valid cache.
func TestResolve_CacheMiss_MalformedFile(t *testing.T) {
	cacheFile := filepath.Join(t.TempDir(), "PINNED_VERSION_TAG")
	if err := os.WriteFile(cacheFile, []byte("garbage with no newlines"), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	var listCalls int32
	r := &Resolver{
		CacheFile: cacheFile,
		ReadCache: func() ([]byte, error) { return os.ReadFile(cacheFile) },
		WriteCache: func(p []byte) error {
			return os.WriteFile(cacheFile, p, 0o644)
		},
		ListReleases: func(_ context.Context) ([]Release, error) {
			atomic.AddInt32(&listCalls, 1)
			return fixtureReleases, nil
		},
	}
	tag, err := r.Resolve(context.Background(), "v0.13.0")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tag != "v2026.5.7" {
		t.Errorf("tag = %q, want v2026.5.7", tag)
	}
	if got := atomic.LoadInt32(&listCalls); got != 1 {
		t.Errorf("malformed cache should not be honoured; got %d list calls", got)
	}
}

// TestResolve_ListError surfaces the underlying error with the package
// prefix and the operator-visible context (which repo).
func TestResolve_ListError(t *testing.T) {
	r := &Resolver{
		ListReleases: func(_ context.Context) ([]Release, error) {
			return nil, errors.New("auth required")
		},
	}
	_, err := r.Resolve(context.Background(), "v0.13.0")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "list releases") {
		t.Errorf("error %q should mention 'list releases'", err.Error())
	}
	if !strings.Contains(err.Error(), "auth required") {
		t.Errorf("error %q should wrap upstream auth message", err.Error())
	}
}

// TestParseReleaseList_RoundTrip is the parse path the production gh
// caller depends on. Verifies that the typed Release survives the JSON
// round-trip including the embedded date in the marketing name.
func TestParseReleaseList_RoundTrip(t *testing.T) {
	raw := []byte(`[
  {"tagName":"v2026.5.16","name":"Hermes Agent v0.14.0 (2026.5.16)"},
  {"tagName":"v2026.5.7","name":"Hermes Agent v0.13.0 (2026.5.7)"}
]`)
	got, err := ParseReleaseList(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].TagName != "v2026.5.16" || got[0].Name != "Hermes Agent v0.14.0 (2026.5.16)" {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1].TagName != "v2026.5.7" || got[1].Name != "Hermes Agent v0.13.0 (2026.5.7)" {
		t.Errorf("got[1] = %+v", got[1])
	}
}

// TestParseCachePayload_TrailingNewlineTolerated locks in the
// cache-file framing contract: missing-trailing-newline is accepted
// because operators occasionally hand-edit the file.
func TestParseCachePayload_TrailingNewlineTolerated(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		wantOK bool
		wantSV string
		wantTG string
	}{
		{name: "canonical", input: "v0.13.0\nv2026.5.7\n", wantOK: true, wantSV: "v0.13.0", wantTG: "v2026.5.7"},
		{name: "no-trailing-newline", input: "v0.13.0\nv2026.5.7", wantOK: true, wantSV: "v0.13.0", wantTG: "v2026.5.7"},
		{name: "extra-blank-lines", input: "v0.13.0\nv2026.5.7\n\n\n", wantOK: true, wantSV: "v0.13.0", wantTG: "v2026.5.7"},
		{name: "single-line", input: "v0.13.0\n", wantOK: false},
		{name: "empty", input: "", wantOK: false},
		{name: "internal-whitespace", input: "v0 13 0\nv2026.5.7\n", wantOK: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			sv, tg, ok := parseCachePayload([]byte(tc.input))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (sv=%q tg=%q)", ok, tc.wantOK, sv, tg)
			}
			if tc.wantOK && (sv != tc.wantSV || tg != tc.wantTG) {
				t.Errorf("parsed = (%q, %q), want (%q, %q)", sv, tg, tc.wantSV, tc.wantTG)
			}
		})
	}
}
