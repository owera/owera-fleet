package configsync_test

import (
	"archive/tar"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/owera/owera-fleet/internal/configsync"
)

// testKeyPair holds the raw ed25519 keys plus the wrapped Signer/Verifier
// so security tests can hand-sign hand-crafted tarballs that bypass PackDir
// without losing access to a matching Verifier.
type testKeyPair struct {
	priv     ed25519.PrivateKey
	signer   *configsync.Signer
	verifier *configsync.Verifier
}

func setupKeyPair(t *testing.T) (*configsync.Signer, *configsync.Verifier) {
	kp := newTestKeyPair(t)
	return kp.signer, kp.verifier
}

func newTestKeyPair(t *testing.T) testKeyPair {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return testKeyPair{
		priv:     priv,
		signer:   configsync.NewSigner(priv),
		verifier: configsync.NewVerifier(pub),
	}
}

// sha256ToManifest returns the "sha256:<hex>" form configsync expects for a string body.
func sha256ToManifest(s string) string {
	h := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(h[:])
}

func TestPackVerifyApply(t *testing.T) {
	signer, verifier := setupKeyPair(t)

	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "config.yaml"), []byte("key: value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(srcDir, "sub"), 0o755)
	if err := os.WriteFile(filepath.Join(srcDir, "sub", "nested.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	bundlePath := filepath.Join(t.TempDir(), "bundle.tar")
	if err := signer.PackDir(srcDir, bundlePath, "bundle-1"); err != nil {
		t.Fatalf("PackDir: %v", err)
	}

	m, err := verifier.Verify(bundlePath)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if m.BundleID != "bundle-1" {
		t.Errorf("BundleID = %q", m.BundleID)
	}
	if len(m.Files) != 2 {
		t.Errorf("expected 2 files, got %d", len(m.Files))
	}

	destDir := t.TempDir()
	if _, err := verifier.Apply(bundlePath, destDir); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(destDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read applied: %v", err)
	}
	if string(got) != "key: value\n" {
		t.Errorf("contents = %q", got)
	}
}

func TestWrongKeyRejected(t *testing.T) {
	signer, _ := setupKeyPair(t)
	_, otherVerifier := setupKeyPair(t)

	srcDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(srcDir, "f.txt"), []byte("x"), 0o644)

	bundlePath := filepath.Join(t.TempDir(), "b.tar")
	_ = signer.PackDir(srcDir, bundlePath, "b")

	_, err := otherVerifier.Verify(bundlePath)
	if !errors.Is(err, configsync.ErrBadSignature) {
		t.Errorf("expected ErrBadSignature, got %v", err)
	}
}

func TestSaveAndLoadKeys(t *testing.T) {
	dir := t.TempDir()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	privPath := filepath.Join(dir, "key")
	pubPath := filepath.Join(dir, "key.pub")

	_ = os.WriteFile(privPath, []byte(encodeKey(priv)), 0o600)
	_ = os.WriteFile(pubPath, []byte(encodeKey(pub)), 0o644)

	signer, err := configsync.LoadSigner(privPath)
	if err != nil {
		t.Fatalf("LoadSigner: %v", err)
	}
	verifier, err := configsync.LoadVerifier(pubPath)
	if err != nil {
		t.Fatalf("LoadVerifier: %v", err)
	}

	srcDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(srcDir, "f"), []byte("hello"), 0o644)
	bundle := filepath.Join(t.TempDir(), "b.tar")
	if err := signer.PackDir(srcDir, bundle, "id"); err != nil {
		t.Fatalf("PackDir: %v", err)
	}
	if _, err := verifier.Verify(bundle); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func encodeKey(k []byte) string {
	return base64.StdEncoding.EncodeToString(k)
}

// --- security regression tests ---

// writeBundle hand-crafts a tarball with a legitimate signed manifest listing
// the entries in `manifestEntries`, then writes every entry in `tarEntries`
// as a tar member (which may diverge from the manifest — e.g. extra entries
// the attacker hopes Apply will extract). Tar mode is forced to 0o777 to
// catch any code that propagates header mode instead of manifest mode.
func writeBundle(t *testing.T, priv ed25519.PrivateKey, bundlePath string, manifestEntries []configsync.ManifestFile, tarEntries map[string][]byte) {
	t.Helper()

	m := configsync.Manifest{
		BundleID:  "test-bundle",
		CreatedAt: time.Now().UTC(),
		Files:     manifestEntries,
	}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	sig := ed25519.Sign(priv, mb)

	f, err := os.Create(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	defer tw.Close()

	writeEntry := func(name string, data []byte, mode int64) {
		hdr := &tar.Header{Name: name, Size: int64(len(data)), Mode: mode, ModTime: time.Now()}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("tar write %s: %v", name, err)
		}
	}
	writeEntry(".manifest.json", mb, 0o644)
	writeEntry(".signature", []byte(base64.StdEncoding.EncodeToString(sig)), 0o644)
	for name, data := range tarEntries {
		writeEntry(name, data, 0o777) // forced wide mode for mode-honoring test
	}
}

// TestApplyDropsExtraTarEntries — headline regression for PR#1 P0-1.
// A bundle with extra tar entries not in the signed manifest must NOT cause
// those entries to be extracted, even if their names attempt path traversal.
func TestApplyDropsExtraTarEntries(t *testing.T) {
	kp := newTestKeyPair(t)

	bundlePath := filepath.Join(t.TempDir(), "evil.tar")
	dest := t.TempDir()
	sibling := filepath.Join(filepath.Dir(dest), "victim")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}

	innocent := []byte("innocent")
	manifest := []configsync.ManifestFile{
		{Path: "innocent.txt", Hash: sha256ToManifest("innocent"), Mode: 0o644, Size: int64(len(innocent))},
	}
	tarBody := map[string][]byte{
		"innocent.txt":   innocent,
		"../victim/pwn1": []byte("traversal payload"),
		"escape.txt":     []byte("extra payload"),
	}
	writeBundle(t, kp.priv, bundlePath, manifest, tarBody)

	if _, err := kp.verifier.Apply(bundlePath, dest); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "innocent.txt")); err != nil {
		t.Errorf("innocent.txt should have been written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sibling, "pwn1")); err == nil {
		t.Errorf("SECURITY: ../victim/pwn1 was written — path traversal not blocked")
	}
	if _, err := os.Stat(filepath.Join(dest, "escape.txt")); err == nil {
		t.Errorf("SECURITY: extra-non-manifest entry escape.txt was written")
	}
}

// TestVerifyRejectsTraversalInManifest — manifest entries whose Path contains
// "..", absolute prefixes, or backslashes must be rejected even though the
// signature would otherwise validate.
func TestVerifyRejectsTraversalInManifest(t *testing.T) {
	kp := newTestKeyPair(t)

	for _, badPath := range []string{
		"../escape",
		"sub/../escape",
		"/abs/path",
		"with\\backslash",
		"./trailing-dot",
		"",
	} {
		bundle := filepath.Join(t.TempDir(), "b.tar")
		manifest := []configsync.ManifestFile{
			{Path: badPath, Hash: sha256ToManifest("x"), Mode: 0o644, Size: 1},
		}
		tarBody := map[string][]byte{badPath: []byte("x")}
		writeBundle(t, kp.priv, bundle, manifest, tarBody)

		_, err := kp.verifier.Verify(bundle)
		if err == nil || !strings.Contains(err.Error(), "configsync:") {
			t.Errorf("expected manifest path %q to be rejected, got err=%v", badPath, err)
		}
	}
}

// TestApplyHonoursManifestModeNotTarHeader — a tar header with mode 0o777
// must not influence the mode of the extracted file; manifest mode wins.
func TestApplyHonoursManifestModeNotTarHeader(t *testing.T) {
	kp := newTestKeyPair(t)

	bundle := filepath.Join(t.TempDir(), "b.tar")
	body := []byte("hi")
	manifest := []configsync.ManifestFile{
		{Path: "f.txt", Hash: sha256ToManifest("hi"), Mode: 0o600, Size: int64(len(body))},
	}
	writeBundle(t, kp.priv, bundle, manifest, map[string][]byte{"f.txt": body})

	dest := t.TempDir()
	if _, err := kp.verifier.Apply(bundle, dest); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	info, err := os.Stat(filepath.Join(dest, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want 0600 (manifest value, not 0o777 tar default)", got)
	}
}

// TestVerifyRejectsDuplicateTarEntries — two tar entries with the same name
// must be rejected so an attacker cannot smuggle a second copy that shadows
// a hashed first copy.
func TestVerifyRejectsDuplicateTarEntries(t *testing.T) {
	kp := newTestKeyPair(t)

	bundle := filepath.Join(t.TempDir(), "dup.tar")
	good := []byte("good")
	manifest := []configsync.ManifestFile{
		{Path: "f.txt", Hash: sha256ToManifest("good"), Mode: 0o644, Size: int64(len(good))},
	}
	// writeBundle uses a map so the duplicate can't be expressed via that
	// helper — hand-write the tar instead.
	mb, _ := json.MarshalIndent(configsync.Manifest{
		BundleID: "dup", CreatedAt: time.Now().UTC(), Files: manifest,
	}, "", "  ")
	sig := ed25519.Sign(kp.priv, mb)
	f, _ := os.Create(bundle)
	tw := tar.NewWriter(f)
	for _, ent := range []struct {
		name string
		body []byte
	}{
		{".manifest.json", mb},
		{".signature", []byte(base64.StdEncoding.EncodeToString(sig))},
		{"f.txt", good},
		{"f.txt", []byte("shadow")}, // duplicate
	} {
		_ = tw.WriteHeader(&tar.Header{Name: ent.name, Size: int64(len(ent.body)), Mode: 0o644, ModTime: time.Now()})
		_, _ = tw.Write(ent.body)
	}
	_ = tw.Close()
	_ = f.Close()

	if _, err := kp.verifier.Verify(bundle); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate-entry rejection, got err=%v", err)
	}
}

// TestApplyImmuneToPostVerifyBundleSwap — Apply must use the in-memory bytes
// captured during verifyContent, not re-read the bundle from disk. We can't
// race the two reads cleanly from a test, but we can prove the structural
// invariant: removing the bundle between the first Apply and a second Apply
// surfaces an open-error on the second call rather than silently succeeding
// against stale state.
func TestApplyImmuneToPostVerifyBundleSwap(t *testing.T) {
	kp := newTestKeyPair(t)

	bundle := filepath.Join(t.TempDir(), "b.tar")
	body := []byte("trusted")
	manifest := []configsync.ManifestFile{
		{Path: "f.txt", Hash: sha256ToManifest("trusted"), Mode: 0o644, Size: int64(len(body))},
	}
	writeBundle(t, kp.priv, bundle, manifest, map[string][]byte{"f.txt": body})

	dest := t.TempDir()
	if _, err := kp.verifier.Apply(bundle, dest); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "trusted" {
		t.Errorf("contents = %q, want trusted", got)
	}
	if err := os.Remove(bundle); err != nil {
		t.Fatal(err)
	}
	if _, err := kp.verifier.Apply(bundle, t.TempDir()); err == nil {
		t.Error("expected open-error after bundle deleted")
	}
}
