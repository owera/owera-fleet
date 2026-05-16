package skillsync_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/owera/owera-fleet/internal/skillsync"
)

// setupKeyPair returns a fresh ed25519 signer + the base64 public key that
// would be pinned on a worker.
func setupKeyPair(t *testing.T) (*skillsync.Signer, []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return skillsync.NewSigner(priv), skillsync.EncodePubKey(pub)
}

// buildSignedRemote constructs a bare git repo on disk, populated with a
// skills directory signed by signer. Returns the remote URL (file:// path)
// the puller will clone from.
//
// The push side is simulated by:
//   - create working tree in a temp dir
//   - drop a couple of skill files under skills/
//   - call signer.SignDir() to write the manifest + signature
//   - git init / add / commit on the working tree
//   - git init --bare on a sibling dir
//   - push the working tree to the bare repo
func buildSignedRemote(t *testing.T, signer *skillsync.Signer, bundleID string, files map[string]string) (remoteURL string) {
	t.Helper()
	base := t.TempDir()
	work := filepath.Join(base, "work")
	bare := filepath.Join(base, "remote.git")

	if err := os.MkdirAll(filepath.Join(work, "skills"), 0o755); err != nil {
		t.Fatalf("mkdir skills: %v", err)
	}
	for name, body := range files {
		p := filepath.Join(work, "skills", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	if _, err := signer.SignDir(filepath.Join(work, "skills"), bundleID); err != nil {
		t.Fatalf("SignDir: %v", err)
	}

	runGit := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// Quiet env so we don't depend on the operator's user.name / .email.
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.com",
			"HOME="+base, // avoid hitting the operator's ~/.gitconfig (signing key, hooks, etc.)
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	runGit(work, "init", "-q", "-b", "main")
	runGit(work, "add", ".")
	runGit(work, "commit", "-q", "-m", "skills: initial sign")
	runGit(work, "init", "-q", "--bare", bare)
	runGit(work, "remote", "add", "origin", bare)
	runGit(work, "push", "-q", "origin", "main")

	return bare
}

// TestPullVerifyApplyHappyPath drives the full clone -> verify -> apply
// pipeline against a freshly-built signed remote.
func TestPullVerifyApplyHappyPath(t *testing.T) {
	signer, pub := setupKeyPair(t)
	remote := buildSignedRemote(t, signer, "bundle-1", map[string]string{
		"alpha/SKILL.md":   "# alpha\n",
		"beta/SKILL.md":    "# beta\n",
		"beta/extra.txt":   "supporting file",
	})

	puller := &skillsync.Puller{
		Source: skillsync.Source{
			GitURL:    remote,
			Branch:    "main",
			PubKey:    pub,
			SkillsDir: "skills",
		},
		LocalDir: filepath.Join(t.TempDir(), "local"),
	}

	m, err := puller.Pull(context.Background())
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if m.BundleID != "bundle-1" {
		t.Errorf("BundleID = %q", m.BundleID)
	}
	if len(m.Files) != 3 {
		t.Errorf("expected 3 files, got %d", len(m.Files))
	}

	dest := filepath.Join(t.TempDir(), "applied")
	applied, unchanged, err := puller.Apply(context.Background(), dest)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if applied != 3 || unchanged != 0 {
		t.Errorf("first Apply: applied=%d unchanged=%d (want 3, 0)", applied, unchanged)
	}
	got, err := os.ReadFile(filepath.Join(dest, "alpha", "SKILL.md"))
	if err != nil {
		t.Fatalf("read applied alpha: %v", err)
	}
	if string(got) != "# alpha\n" {
		t.Errorf("alpha contents = %q", got)
	}
}

// TestApplyIdempotent re-Applies onto a destination that already has the
// same content; the second pass should report unchanged for every file.
func TestApplyIdempotent(t *testing.T) {
	signer, pub := setupKeyPair(t)
	remote := buildSignedRemote(t, signer, "id-2", map[string]string{
		"one/SKILL.md": "one",
		"two/SKILL.md": "two",
	})
	puller := &skillsync.Puller{
		Source: skillsync.Source{
			GitURL:    remote,
			Branch:    "main",
			PubKey:    pub,
			SkillsDir: "skills",
		},
		LocalDir: filepath.Join(t.TempDir(), "local"),
	}
	if _, err := puller.Pull(context.Background()); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "applied")
	if applied, unchanged, err := puller.Apply(context.Background(), dest); err != nil {
		t.Fatalf("Apply #1: %v", err)
	} else if applied != 2 || unchanged != 0 {
		t.Errorf("Apply #1: applied=%d unchanged=%d (want 2, 0)", applied, unchanged)
	}
	if applied, unchanged, err := puller.Apply(context.Background(), dest); err != nil {
		t.Fatalf("Apply #2: %v", err)
	} else if applied != 0 || unchanged != 2 {
		t.Errorf("Apply #2: applied=%d unchanged=%d (want 0, 2)", applied, unchanged)
	}
}

// TestTamperedManifestRejected verifies that any post-signing modification
// to the manifest itself (e.g. swapping in a different BundleID) trips the
// signature check.
func TestTamperedManifestRejected(t *testing.T) {
	signer, pub := setupKeyPair(t)
	remote := buildSignedRemote(t, signer, "good", map[string]string{
		"x/SKILL.md": "x",
	})
	puller := &skillsync.Puller{
		Source: skillsync.Source{
			GitURL:    remote,
			Branch:    "main",
			PubKey:    pub,
			SkillsDir: "skills",
		},
		LocalDir: filepath.Join(t.TempDir(), "local"),
	}
	// First pull populates LocalDir.
	if _, err := puller.Pull(context.Background()); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	// Tamper with the manifest on disk (post-Pull, pre-Apply). Replacing
	// even one byte must break the signature.
	manifestPath := filepath.Join(puller.LocalDir, "skills", skillsync.ManifestName)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	// Flip a byte deep in the file rather than truncating — we want the
	// JSON to remain parseable so the failure mode is the signature itself.
	data[len(data)/2] ^= 0x01
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatalf("write tampered manifest: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "applied")
	if _, _, err := puller.Apply(context.Background(), dest); !errors.Is(err, skillsync.ErrBadSignature) {
		t.Errorf("expected ErrBadSignature, got %v", err)
	}
}

// TestHashMismatchRejected tampers with a content file after signing. The
// manifest+signature still verify, but the file's hash no longer matches.
func TestHashMismatchRejected(t *testing.T) {
	signer, pub := setupKeyPair(t)
	remote := buildSignedRemote(t, signer, "good", map[string]string{
		"x/SKILL.md": "original",
	})
	puller := &skillsync.Puller{
		Source: skillsync.Source{
			GitURL:    remote,
			Branch:    "main",
			PubKey:    pub,
			SkillsDir: "skills",
		},
		LocalDir: filepath.Join(t.TempDir(), "local"),
	}
	if _, err := puller.Pull(context.Background()); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	// Replace the file content without re-signing.
	tamperedPath := filepath.Join(puller.LocalDir, "skills", "x", "SKILL.md")
	if err := os.WriteFile(tamperedPath, []byte("tampered"), 0o644); err != nil {
		t.Fatalf("write tampered file: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "applied")
	if _, _, err := puller.Apply(context.Background(), dest); !errors.Is(err, skillsync.ErrHashMismatch) {
		t.Errorf("expected ErrHashMismatch, got %v", err)
	}
}

// TestWrongKeyRejected pulls with a verifier key that doesn't match the
// signing key. The on-disk manifest is well-formed; the signature just
// doesn't verify under the wrong public key.
func TestWrongKeyRejected(t *testing.T) {
	signer, _ := setupKeyPair(t)
	_, otherPub := setupKeyPair(t)

	remote := buildSignedRemote(t, signer, "good", map[string]string{
		"a/SKILL.md": "a",
	})
	puller := &skillsync.Puller{
		Source: skillsync.Source{
			GitURL:    remote,
			Branch:    "main",
			PubKey:    otherPub,
			SkillsDir: "skills",
		},
		LocalDir: filepath.Join(t.TempDir(), "local"),
	}
	if _, err := puller.Pull(context.Background()); !errors.Is(err, skillsync.ErrBadSignature) {
		t.Errorf("expected ErrBadSignature, got %v", err)
	}
}

// TestRePullPullsLatest seeds a remote, pulls once, then advances the
// remote with a second signed commit. A second Pull on the same LocalDir
// must observe the new bundle.
func TestRePullPullsLatest(t *testing.T) {
	signer, pub := setupKeyPair(t)

	// Build a remote with bundle v1.
	base := t.TempDir()
	work := filepath.Join(base, "work")
	bare := filepath.Join(base, "remote.git")
	if err := os.MkdirAll(filepath.Join(work, "skills", "v"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "skills", "v", "SKILL.md"), []byte("v1"), 0o644); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	if _, err := signer.SignDir(filepath.Join(work, "skills"), "v1"); err != nil {
		t.Fatalf("SignDir v1: %v", err)
	}
	runGit := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
			"HOME="+base,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit(work, "init", "-q", "-b", "main")
	runGit(work, "add", ".")
	runGit(work, "commit", "-q", "-m", "v1")
	runGit(work, "init", "-q", "--bare", bare)
	runGit(work, "remote", "add", "origin", bare)
	runGit(work, "push", "-q", "origin", "main")

	puller := &skillsync.Puller{
		Source: skillsync.Source{
			GitURL:    bare,
			Branch:    "main",
			PubKey:    pub,
			SkillsDir: "skills",
		},
		LocalDir: filepath.Join(t.TempDir(), "local"),
	}
	m, err := puller.Pull(context.Background())
	if err != nil {
		t.Fatalf("Pull v1: %v", err)
	}
	if m.BundleID != "v1" {
		t.Fatalf("first pull bundle = %q", m.BundleID)
	}

	// Now bump the working tree to v2 and push.
	if err := os.WriteFile(filepath.Join(work, "skills", "v", "SKILL.md"), []byte("v2"), 0o644); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	if _, err := signer.SignDir(filepath.Join(work, "skills"), "v2"); err != nil {
		t.Fatalf("SignDir v2: %v", err)
	}
	runGit(work, "add", ".")
	runGit(work, "commit", "-q", "-m", "v2")
	runGit(work, "push", "-q", "origin", "main")

	m2, err := puller.Pull(context.Background())
	if err != nil {
		t.Fatalf("Pull v2: %v", err)
	}
	if m2.BundleID != "v2" {
		t.Errorf("second pull bundle = %q, want v2", m2.BundleID)
	}
}

// TestLoadSignerRoundTrip exercises the disk key load + use path so we
// catch any base64 / padding regressions independent of GenerateKey.
func TestLoadSignerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	keyPath := filepath.Join(dir, "signing.key")
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(priv)), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	signer, err := skillsync.LoadSigner(keyPath)
	if err != nil {
		t.Fatalf("LoadSigner: %v", err)
	}
	if !ed25519.PublicKey(signer.Public()).Equal(pub) {
		t.Errorf("loaded signer public key does not match")
	}
}
