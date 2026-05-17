// Package configsync signs config bundles on the gateway and verifies them
// on worker nodes against a pinned ed25519 public key. The push side computes
// a manifest of {path → sha256, mode} entries for every file in the bundle,
// signs the manifest, and tarballs the bundle + manifest + signature. The
// verify side untars, re-hashes every file, and rejects the bundle if either
// the signature does not verify or any file's hash differs from the manifest.
//
// This is the §4 signed-config-sync primitive — the operator plane's
// equivalent of a code-signed install bundle.
package configsync

import (
	"archive/tar"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Manifest is the signed listing of files in a config bundle.
type Manifest struct {
	BundleID  string         `json:"bundle_id"`
	CreatedAt time.Time      `json:"created_at"`
	Files     []ManifestFile `json:"files"`
}

// ManifestFile describes one file in the bundle.
type ManifestFile struct {
	Path string `json:"path"`
	Hash string `json:"hash"` // sha256:<hex>
	Mode uint32 `json:"mode"`
	Size int64  `json:"size"`
}

// On-wire constants for the signed bundle.
const (
	manifestName  = ".manifest.json"
	signatureName = ".signature"
)

// Signer wraps the gateway-side signing key.
type Signer struct {
	priv ed25519.PrivateKey
}

// NewSigner returns a Signer using the supplied private key. Use
// [LoadSigner] to read a key file from disk instead.
func NewSigner(priv ed25519.PrivateKey) *Signer {
	return &Signer{priv: priv}
}

// LoadSigner reads a base64-encoded ed25519 private key from path.
func LoadSigner(path string) (*Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("configsync: read signing key %s: %w", path, err)
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("configsync: decode signing key: %w", err)
	}
	if len(dec) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("configsync: bad signing key size %d", len(dec))
	}
	return &Signer{priv: ed25519.PrivateKey(dec)}, nil
}

// Public returns the corresponding public key for distribution to verifiers.
func (s *Signer) Public() ed25519.PublicKey {
	return s.priv.Public().(ed25519.PublicKey)
}

// PackDir creates a signed tarball of every regular file under srcDir,
// writing it to outPath. File paths in the manifest are relative to srcDir.
func (s *Signer) PackDir(srcDir, outPath, bundleID string) error {
	var files []ManifestFile

	err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(srcDir, path)
		// Don't ever include the reserved names.
		if rel == manifestName || rel == signatureName {
			return fmt.Errorf("configsync: source contains reserved filename %s", rel)
		}
		hash, size, err := hashFile(path)
		if err != nil {
			return err
		}
		info, _ := d.Info()
		files = append(files, ManifestFile{
			Path: rel,
			Hash: hash,
			Mode: uint32(info.Mode().Perm()),
			Size: size,
		})
		return nil
	})
	if err != nil {
		return fmt.Errorf("configsync: walk %s: %w", srcDir, err)
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	m := Manifest{BundleID: bundleID, CreatedAt: time.Now().UTC(), Files: files}

	manifestBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("configsync: marshal manifest: %w", err)
	}
	sig := ed25519.Sign(s.priv, manifestBytes)

	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("configsync: create %s: %w", outPath, err)
	}
	defer out.Close()

	tw := tar.NewWriter(out)
	defer tw.Close()

	// Manifest + signature first.
	if err := writeTarEntry(tw, manifestName, manifestBytes, 0o644); err != nil {
		return err
	}
	if err := writeTarEntry(tw, signatureName, []byte(base64.StdEncoding.EncodeToString(sig)), 0o644); err != nil {
		return err
	}

	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(srcDir, f.Path))
		if err != nil {
			return fmt.Errorf("configsync: read %s: %w", f.Path, err)
		}
		if err := writeTarEntry(tw, f.Path, data, fs.FileMode(f.Mode)); err != nil {
			return err
		}
	}
	return nil
}

// Verifier wraps a pinned public key.
type Verifier struct {
	pub ed25519.PublicKey
}

// NewVerifier returns a Verifier using the supplied public key.
func NewVerifier(pub ed25519.PublicKey) *Verifier {
	return &Verifier{pub: pub}
}

// LoadVerifier reads a base64-encoded public key from path.
func LoadVerifier(path string) (*Verifier, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("configsync: read pubkey %s: %w", path, err)
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("configsync: decode pubkey: %w", err)
	}
	if len(dec) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("configsync: bad pubkey size %d", len(dec))
	}
	return &Verifier{pub: ed25519.PublicKey(dec)}, nil
}

// ErrBadSignature is returned when the manifest signature does not verify.
var ErrBadSignature = errors.New("configsync: signature verification failed")

// ErrHashMismatch is returned when a file's contents do not match the manifest.
var ErrHashMismatch = errors.New("configsync: file hash mismatch")

// Verify reads a signed bundle from bundlePath, verifies the manifest signature
// against the verifier's public key, and checks every file's hash. Returns the
// manifest on success.
func (v *Verifier) Verify(bundlePath string) (*Manifest, error) {
	m, _, err := v.verifyContent(bundlePath)
	return m, err
}

// verifyContent reads the bundle once, verifies the signature, validates every
// manifest path is a clean relative path, and returns the per-file content
// map keyed by manifest path alongside the manifest itself. Apply consumes
// this map directly so it never re-reads the bundle from disk — closing the
// TOCTOU window between verification and extraction.
func (v *Verifier) verifyContent(bundlePath string) (*Manifest, map[string][]byte, error) {
	f, err := os.Open(bundlePath)
	if err != nil {
		return nil, nil, fmt.Errorf("configsync: open bundle: %w", err)
	}
	defer f.Close()

	var manifestBytes, sigBytes []byte
	fileData := make(map[string][]byte)

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("configsync: read tar: %w", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, nil, fmt.Errorf("configsync: read entry %s: %w", hdr.Name, err)
		}
		switch hdr.Name {
		case manifestName:
			manifestBytes = data
		case signatureName:
			sigBytes = data
		default:
			// Reject duplicate entries — a second copy of the same name with
			// different content would shadow the verified first copy.
			if _, dup := fileData[hdr.Name]; dup {
				return nil, nil, fmt.Errorf("configsync: duplicate tar entry %s", hdr.Name)
			}
			fileData[hdr.Name] = data
		}
	}

	if manifestBytes == nil || sigBytes == nil {
		return nil, nil, fmt.Errorf("configsync: bundle missing manifest or signature")
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigBytes)))
	if err != nil {
		return nil, nil, fmt.Errorf("configsync: decode signature: %w", err)
	}
	if !ed25519.Verify(v.pub, manifestBytes, sig) {
		return nil, nil, ErrBadSignature
	}

	var m Manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return nil, nil, fmt.Errorf("configsync: parse manifest: %w", err)
	}

	// Build the verified content map. Tar entries not listed in the
	// manifest are dropped here and never reach Apply, even though
	// the tar may contain them (signature only covers the manifest).
	verified := make(map[string][]byte, len(m.Files))
	for _, mf := range m.Files {
		if err := validateManifestPath(mf.Path); err != nil {
			return nil, nil, err
		}
		data, ok := fileData[mf.Path]
		if !ok {
			return nil, nil, fmt.Errorf("configsync: bundle missing %s", mf.Path)
		}
		got := "sha256:" + hex.EncodeToString(sha256Sum(data))
		if got != mf.Hash {
			return nil, nil, fmt.Errorf("%w: %s: %s != %s", ErrHashMismatch, mf.Path, got, mf.Hash)
		}
		verified[mf.Path] = data
	}
	return &m, verified, nil
}

// validateManifestPath rejects absolute paths, backslashes, colons, and any
// path containing "..", "." segments, or empty segments. Manifest paths on
// the wire always use "/" separators (the packer runs filepath.Rel on the
// gateway, which produces "/" on darwin); rejecting backslash defends
// against bundles authored on other platforms.
func validateManifestPath(p string) error {
	if p == "" {
		return fmt.Errorf("configsync: manifest entry has empty path")
	}
	if strings.ContainsAny(p, `\:`) {
		return fmt.Errorf("configsync: manifest path contains invalid char: %q", p)
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		return fmt.Errorf("configsync: manifest path is absolute: %q", p)
	}
	if cleaned := filepath.ToSlash(filepath.Clean(p)); cleaned != p {
		return fmt.Errorf("configsync: manifest path is not canonical: %q (would become %q)", p, cleaned)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return fmt.Errorf("configsync: manifest path has bad segment %q in %q", seg, p)
		}
	}
	return nil
}

// Apply unpacks a verified bundle into destDir. Verification is run first;
// if it fails, no files are written. Apply uses the in-memory content map
// captured during verification — it does not re-read the bundle from disk,
// so the bundle can be removed or replaced between Verify and Apply without
// affecting what gets written.
func (v *Verifier) Apply(bundlePath, destDir string) (*Manifest, error) {
	m, content, err := v.verifyContent(bundlePath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("configsync: mkdir dest: %w", err)
	}
	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return nil, fmt.Errorf("configsync: resolve dest: %w", err)
	}

	for _, mf := range m.Files {
		outPath := filepath.Join(destAbs, filepath.FromSlash(mf.Path))
		// Defence-in-depth: validateManifestPath already rejected "..",
		// but re-check the resolved path stays under destAbs.
		rel, err := filepath.Rel(destAbs, outPath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("configsync: refusing to write outside dest: %s", mf.Path)
		}
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return nil, fmt.Errorf("configsync: mkdir: %w", err)
		}
		// Mode comes from the signed manifest, never from a tar header.
		mode := fs.FileMode(mf.Mode).Perm()
		if err := os.WriteFile(outPath, content[mf.Path], mode); err != nil {
			return nil, fmt.Errorf("configsync: write %s: %w", outPath, err)
		}
	}
	return m, nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), n, nil
}

func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func writeTarEntry(tw *tar.Writer, name string, data []byte, mode fs.FileMode) error {
	hdr := &tar.Header{
		Name:    name,
		Size:    int64(len(data)),
		Mode:    int64(mode.Perm()),
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("configsync: tar header %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("configsync: tar write %s: %w", name, err)
	}
	return nil
}
