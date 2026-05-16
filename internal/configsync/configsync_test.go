package configsync_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/owera/owera-fleet/internal/configsync"
)

func setupKeyPair(t *testing.T) (*configsync.Signer, *configsync.Verifier) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return configsync.NewSigner(priv), configsync.NewVerifier(pub)
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
