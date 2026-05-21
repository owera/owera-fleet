//go:build integration

// Integration test guarded behind the `integration` build tag so it
// only runs when an operator explicitly asks for it (e.g. `go test
// -tags=integration ./internal/hermesrelease`). Reaches the live
// GitHub API via `gh release list`, so it requires `gh` on PATH and
// network access. Used to confirm the production wiring of
// Default() against the real Nous Research release list during
// issue #52 verification.
package hermesrelease

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestDefault_LiveLookup is the live-verification check the issue #52
// acceptance criterion calls out: v0.13.0 must resolve to the CalVer
// tag v2026.5.7 against the actual NousResearch/hermes-agent release
// list. Caches into a temp file (not the operator's real
// ~/.hermes/PINNED_VERSION_TAG) so re-running the test doesn't
// pollute production state.
func TestDefault_LiveLookup(t *testing.T) {
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh not on PATH")
	}
	tmp := filepath.Join(t.TempDir(), "PINNED_VERSION_TAG")
	r := &Resolver{
		CacheFile:    tmp,
		Repo:         DefaultRepo,
		ListLimit:    DefaultListLimit,
		LookupTimeout: DefaultLookupTimeout,
		ListReleases: productionListReleases,
		ReadCache:    func() ([]byte, error) { return os.ReadFile(tmp) },
		WriteCache:   func(p []byte) error { return os.WriteFile(tmp, p, 0o644) },
	}
	got, err := r.Resolve(context.Background(), "v0.13.0")
	if err != nil {
		t.Fatalf("live Resolve: %v", err)
	}
	if got != "v2026.5.7" {
		t.Errorf("live v0.13.0 -> %q, want v2026.5.7", got)
	}
}
