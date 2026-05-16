// Package skillsync pulls skill bundles from a signed remote Git source and
// applies them onto worker nodes, transferring trust from the gateway to
// workers via a pinned ed25519 public key.
//
// The push side (the gateway) generates `.skills-manifest.json` covering
// every file under the bundle's skills directory and signs it with an
// ed25519 private key. The signature lives next to the manifest in
// `.skills-signature` (base64). Workers clone-or-fetch the remote, verify
// the signature against their pinned public key, re-hash every file in the
// manifest, and refuse to apply if anything diverges. Apply is atomic
// per-file (write-tmp + rename) so a half-applied bundle never appears on
// disk.
//
// This is the §4 signed-skill-sync primitive — the agent plane's
// counterpart to configsync. The on-disk layouts differ (configsync uses a
// tarball, skillsync uses a Git checkout) but the signing-and-verify
// pattern is intentionally identical so a future operator can hold one
// mental model.
package skillsync

import (
	"context"
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
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Reserved manifest + signature filenames at the root of the bundle's
// SkillsDir. Skill content with these basenames is rejected at sign time.
const (
	ManifestName  = ".skills-manifest.json"
	SignatureName = ".skills-signature"
)

// Source describes a signed-Git skill bundle.
//
// GitURL is the remote to clone (https or git+ssh). Branch defaults to
// "main" when empty. PubKey is the base64-encoded ed25519 public key the
// worker pins for verification — never carried in band. SkillsDir is the
// path inside the repo containing the bundle's content; ManifestName +
// SignatureName live at that root.
type Source struct {
	GitURL    string
	Branch    string
	PubKey    []byte
	SkillsDir string
}

// SkillFile describes one file in a signed bundle.
type SkillFile struct {
	Path string `json:"path"` // relative to SkillsDir
	Hash string `json:"hash"` // sha256:<hex>
	Mode uint32 `json:"mode"`
	Size int64  `json:"size"`
}

// Manifest is the signed listing covering every file in the bundle's
// SkillsDir. Marshalled JSON is the exact byte sequence ed25519-signed by
// the gateway; do not reorder fields without bumping a bundle version.
type Manifest struct {
	BundleID  string      `json:"bundle_id"`
	CreatedAt time.Time   `json:"created_at"`
	Files     []SkillFile `json:"files"`
}

// GitRunner is the shape of the git shell-out used by Puller. Returns the
// command's stdout, its exit code, and an error for transport-level
// failures (couldn't start git). A non-zero exit code is not, on its own,
// returned as an error — callers inspect the code so failure modes like
// "remote unreachable" can be distinguished from "git not installed".
//
// Injectable so tests can drive a local bare repo without exec.
type GitRunner func(ctx context.Context, args ...string) (stdout string, code int, err error)

// DefaultGitRunner shells out to the system `git` binary. Stderr is
// captured into the returned error when git itself fails to start; stdout
// is returned as a string.
func DefaultGitRunner(ctx context.Context, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		// exec.ExitError means git ran and exited non-zero; surface the code
		// but keep err == nil so the caller can decide what to do.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return stdout.String(), exitErr.ExitCode(),
				fmt.Errorf("skillsync: git %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
		}
		return stdout.String(), -1,
			fmt.Errorf("skillsync: invoke git %s: %w", strings.Join(args, " "), err)
	}
	return stdout.String(), 0, nil
}

// Puller clones-or-fetches a signed Git skill bundle into LocalDir,
// verifies it against Source.PubKey, and applies it to a destination
// directory on demand.
//
// LocalDir is the persistent working tree the puller maintains across
// calls; the first Pull clones into it, subsequent Pulls fetch + checkout.
// Callers are responsible for picking a path under ~/.hermes/ — Puller
// makes no assumptions.
type Puller struct {
	GitRunner GitRunner
	Source    Source
	LocalDir  string
}

// ErrBadSignature is returned when the manifest's ed25519 signature does
// not verify against Source.PubKey.
var ErrBadSignature = errors.New("skillsync: signature verification failed")

// ErrHashMismatch is returned when a file in the bundle does not match
// the hash recorded in the manifest.
var ErrHashMismatch = errors.New("skillsync: file hash mismatch")

// ErrMissingManifest is returned when the bundle has no .skills-manifest.json.
var ErrMissingManifest = errors.New("skillsync: manifest missing from bundle")

// ErrMissingSignature is returned when the bundle has no .skills-signature.
var ErrMissingSignature = errors.New("skillsync: signature missing from bundle")

// ErrBadPubKey is returned when Source.PubKey is missing or not a valid
// base64-encoded ed25519 public key.
var ErrBadPubKey = errors.New("skillsync: bad public key")

// Pull clones-or-fetches the configured remote into LocalDir, then reads
// and verifies the manifest + signature. On success it returns the parsed
// manifest. On failure no application state is touched.
//
// The clone uses --depth 1 against Source.Branch (default "main") to keep
// the worker working tree small. A re-pull does `git fetch` + a hard
// checkout of origin/<branch> rather than `git pull`, so a force-pushed
// remote (e.g. signing-key rotation) doesn't leave a merge conflict on the
// worker.
func (p *Puller) Pull(ctx context.Context) (*Manifest, error) {
	if p.Source.GitURL == "" {
		return nil, errors.New("skillsync: Source.GitURL is required")
	}
	if p.LocalDir == "" {
		return nil, errors.New("skillsync: LocalDir is required")
	}
	branch := p.Source.Branch
	if branch == "" {
		branch = "main"
	}
	run := p.GitRunner
	if run == nil {
		run = DefaultGitRunner
	}

	// Parent dir must exist for both clone and fetch paths.
	if err := os.MkdirAll(filepath.Dir(p.LocalDir), 0o755); err != nil {
		return nil, fmt.Errorf("skillsync: mkdir parent: %w", err)
	}

	// Probe for an existing checkout. Anything that isn't a git working
	// tree gets re-cloned from scratch — defensive against a half-clone
	// left by an interrupted previous Pull.
	if isWorkingTree(p.LocalDir) {
		// fetch + hard reset to origin/<branch>.
		if _, code, err := run(ctx, "-C", p.LocalDir, "fetch", "--depth=1", "origin", branch); err != nil || code != 0 {
			return nil, fmt.Errorf("skillsync: fetch: %w", err)
		}
		if _, code, err := run(ctx, "-C", p.LocalDir, "checkout", "-f", "FETCH_HEAD"); err != nil || code != 0 {
			return nil, fmt.Errorf("skillsync: checkout: %w", err)
		}
	} else {
		// Wipe any stale partial directory then clone.
		if err := os.RemoveAll(p.LocalDir); err != nil {
			return nil, fmt.Errorf("skillsync: remove stale local: %w", err)
		}
		if _, code, err := run(ctx, "clone", "--depth=1", "--branch", branch, p.Source.GitURL, p.LocalDir); err != nil || code != 0 {
			return nil, fmt.Errorf("skillsync: clone: %w", err)
		}
	}

	return p.verifyOnDisk()
}

// Apply copies the verified skill files into destDir, writing each one
// atomically (write-to-temp + rename). Returns the count of files that
// were freshly written and the count that were already up-to-date (same
// hash on disk, no rewrite issued). Apply re-verifies the bundle before
// touching destDir so a tampered working tree between Pull and Apply is
// still caught.
func (p *Puller) Apply(ctx context.Context, destDir string) (applied, unchanged int, err error) {
	m, err := p.verifyOnDisk()
	if err != nil {
		return 0, 0, err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return 0, 0, fmt.Errorf("skillsync: mkdir dest: %w", err)
	}
	bundleRoot := filepath.Join(p.LocalDir, p.Source.SkillsDir)
	for _, f := range m.Files {
		srcPath := filepath.Join(bundleRoot, f.Path)
		dstPath := filepath.Join(destDir, f.Path)

		// Skip the rewrite if the destination already matches the hash —
		// this lets the operator run pull --apply on a cron and get a
		// truthful "no-change" signal in logs.
		if existing, _, hashErr := hashFile(dstPath); hashErr == nil && existing == f.Hash {
			unchanged++
			continue
		}

		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			return applied, unchanged, fmt.Errorf("skillsync: mkdir %s: %w", filepath.Dir(dstPath), err)
		}
		if err := copyFileAtomic(srcPath, dstPath, fs.FileMode(f.Mode).Perm()); err != nil {
			return applied, unchanged, err
		}
		applied++
	}
	return applied, unchanged, nil
}

// verifyOnDisk reads .skills-manifest.json + .skills-signature out of the
// working tree at LocalDir/SkillsDir, verifies the signature against
// Source.PubKey, then re-hashes every listed file. Returns the manifest
// on success.
func (p *Puller) verifyOnDisk() (*Manifest, error) {
	pub, err := decodePubKey(p.Source.PubKey)
	if err != nil {
		return nil, err
	}
	bundleRoot := filepath.Join(p.LocalDir, p.Source.SkillsDir)

	manifestBytes, err := os.ReadFile(filepath.Join(bundleRoot, ManifestName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrMissingManifest
		}
		return nil, fmt.Errorf("skillsync: read manifest: %w", err)
	}
	sigBytes, err := os.ReadFile(filepath.Join(bundleRoot, SignatureName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrMissingSignature
		}
		return nil, fmt.Errorf("skillsync: read signature: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigBytes)))
	if err != nil {
		return nil, fmt.Errorf("skillsync: decode signature: %w", err)
	}
	if !ed25519.Verify(pub, manifestBytes, sig) {
		return nil, ErrBadSignature
	}

	var m Manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return nil, fmt.Errorf("skillsync: parse manifest: %w", err)
	}

	for _, f := range m.Files {
		if f.Path == ManifestName || f.Path == SignatureName {
			// Defensive: the manifest must never list itself or its
			// signature; if a malicious signer tries to do so we treat the
			// bundle as malformed.
			return nil, fmt.Errorf("skillsync: manifest lists reserved file %s", f.Path)
		}
		got, _, err := hashFile(filepath.Join(bundleRoot, f.Path))
		if err != nil {
			return nil, fmt.Errorf("skillsync: hash %s: %w", f.Path, err)
		}
		if got != f.Hash {
			return nil, fmt.Errorf("%w: %s: %s != %s", ErrHashMismatch, f.Path, got, f.Hash)
		}
	}
	return &m, nil
}

// Signer wraps the gateway-side signing key for producing skillsync
// bundles. The CLI's `skill-sync sign` subcommand uses this.
type Signer struct {
	priv ed25519.PrivateKey
}

// NewSigner returns a Signer using the supplied private key.
func NewSigner(priv ed25519.PrivateKey) *Signer {
	return &Signer{priv: priv}
}

// LoadSigner reads a base64-encoded ed25519 private key from path.
func LoadSigner(path string) (*Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("skillsync: read signing key %s: %w", path, err)
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("skillsync: decode signing key: %w", err)
	}
	if len(dec) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("skillsync: bad signing key size %d", len(dec))
	}
	return &Signer{priv: ed25519.PrivateKey(dec)}, nil
}

// Public returns the corresponding public key for distribution.
func (s *Signer) Public() ed25519.PublicKey {
	return s.priv.Public().(ed25519.PublicKey)
}

// SignDir walks srcDir, computes sha256 hashes for every regular file,
// builds a Manifest, signs it, and writes the manifest + signature into
// srcDir. The signed bundle is then ready to be committed to the remote
// Git repo. bundleID is recorded verbatim in the manifest.
//
// The reserved filenames (ManifestName, SignatureName) are excluded from
// the walk so re-signing an already-signed directory produces a fresh
// manifest covering only the real content.
func (s *Signer) SignDir(srcDir, bundleID string) (*Manifest, error) {
	var files []SkillFile

	err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(srcDir, path)
		if relErr != nil {
			return relErr
		}
		// Skip our own metadata; otherwise the manifest would list itself.
		if rel == ManifestName || rel == SignatureName {
			return nil
		}
		hash, size, hErr := hashFile(path)
		if hErr != nil {
			return hErr
		}
		info, _ := d.Info()
		files = append(files, SkillFile{
			Path: rel,
			Hash: hash,
			Mode: uint32(info.Mode().Perm()),
			Size: size,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("skillsync: walk %s: %w", srcDir, err)
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	m := Manifest{BundleID: bundleID, CreatedAt: time.Now().UTC(), Files: files}

	manifestBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("skillsync: marshal manifest: %w", err)
	}
	sig := ed25519.Sign(s.priv, manifestBytes)

	if err := os.WriteFile(filepath.Join(srcDir, ManifestName), manifestBytes, 0o644); err != nil {
		return nil, fmt.Errorf("skillsync: write manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, SignatureName), []byte(base64.StdEncoding.EncodeToString(sig)), 0o644); err != nil {
		return nil, fmt.Errorf("skillsync: write signature: %w", err)
	}
	return &m, nil
}

// EncodePubKey returns the base64 form of pub used for storage on disk
// and as the Source.PubKey wire format.
func EncodePubKey(pub ed25519.PublicKey) []byte {
	return []byte(base64.StdEncoding.EncodeToString(pub))
}

func decodePubKey(b []byte) (ed25519.PublicKey, error) {
	if len(b) == 0 {
		return nil, ErrBadPubKey
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadPubKey, err)
	}
	if len(dec) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: size %d", ErrBadPubKey, len(dec))
	}
	return ed25519.PublicKey(dec), nil
}

// hashFile returns sha256:<hex> for the file at path.
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

// isWorkingTree reports whether dir is a git working tree we can fetch
// against. A plain directory or a partially-cloned directory returns
// false — Pull will rm -rf and re-clone in that case.
func isWorkingTree(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil {
		return false
	}
	// `.git` may be a directory (normal clone) or a regular file (git
	// worktrees). Either is acceptable.
	return st.IsDir() || st.Mode().IsRegular()
}

// copyFileAtomic writes src to dst via a same-directory temp file +
// rename. The temp file is created with the final mode so the rename
// publishes the file at its intended permissions in one step. A failed
// write leaves the original dst untouched.
func copyFileAtomic(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("skillsync: open src %s: %w", src, err)
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return fmt.Errorf("skillsync: create temp: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we fail before the rename.
	defer func() {
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return fmt.Errorf("skillsync: copy %s: %w", src, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("skillsync: chmod %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("skillsync: close tmp: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("skillsync: rename %s -> %s: %w", tmpName, dst, err)
	}
	return nil
}
