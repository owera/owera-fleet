// Package ledger maintains a signed JSONL audit trail per task-id.
//
// Each entry is serialized as JSON, signed with an ed25519 key stored in the
// ledger directory, and appended to ~/.hermes/ledger/<task-id>.jsonl. On read
// every entry's signature is verified against the stored public key; tampering
// or truncation is surfaced as a distinct error.
//
// The key pair is auto-generated on first [Open] and written to
// dir/.signing-key (mode 0o600) and dir/.public-key (mode 0o644). Operators
// should back up .signing-key alongside the restic snapshot; loss means
// historical entries cannot be verified (but can still be read).
package ledger

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ResultBill is a ledger-specific result constant for billing events.
// It coexists with the log.Result constants but is ledger-domain only.
const (
	ResultOK      = "ok"
	ResultError   = "error"
	ResultBill    = "bill"
	ResultMarker  = "marker"
	ResultSkipped = "skipped"
)

// Entry is one signed line in a task ledger file.
//
// Sig is the base64-encoded ed25519 signature over the canonical JSON of the
// entry with the Sig field set to "" (omitempty drops it from the signed
// payload). The zero value of Entry is not valid; callers must set TaskID and
// Phase at minimum.
//
// TenantID is the customer-plane tenant on whose behalf the work was
// performed. It is `omitempty` so operator-only jobs (no tenant) continue to
// produce the same signed bytes as before this field was introduced —
// pre-existing ledger files therefore still verify cleanly after upgrade.
// When set, the value is covered by the signature like every other field.
type Entry struct {
	Ts         time.Time       `json:"ts"`
	TaskID     string          `json:"task_id"`
	TenantID   string          `json:"tenant_id,omitempty"`
	Phase      string          `json:"phase"`
	Action     string          `json:"action"`
	Result     string          `json:"result"`
	Data       json.RawMessage `json:"data,omitempty"`
	DurationMs int64           `json:"duration_ms,omitempty"`
	StderrTail string          `json:"stderr_tail,omitempty"`
	Sig        string          `json:"sig,omitempty"`
}

// Ledger writes and reads signed entries rooted at a directory.
type Ledger struct {
	dir  string
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

const (
	privKeyFile = ".signing-key"
	pubKeyFile  = ".public-key"
)

// Open returns a Ledger rooted at dir. If the key pair does not exist it is
// generated and written to disk. Callers that only need to read+verify without
// writing may pass an empty dir and call [OpenReadOnly] instead.
func Open(dir string) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("ledger: mkdir %s: %w", dir, err)
	}
	privPath := filepath.Join(dir, privKeyFile)
	pubPath := filepath.Join(dir, pubKeyFile)

	var priv ed25519.PrivateKey
	var pub ed25519.PublicKey

	privBytes, err := os.ReadFile(privPath)
	if errors.Is(err, os.ErrNotExist) {
		pub, priv, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("ledger: generate key: %w", err)
		}
		if err := os.WriteFile(privPath, []byte(base64.StdEncoding.EncodeToString(priv)), 0o600); err != nil {
			return nil, fmt.Errorf("ledger: write signing key: %w", err)
		}
		if err := os.WriteFile(pubPath, []byte(base64.StdEncoding.EncodeToString(pub)), 0o644); err != nil {
			return nil, fmt.Errorf("ledger: write public key: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("ledger: read signing key: %w", err)
	} else {
		privDec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(privBytes)))
		if err != nil {
			return nil, fmt.Errorf("ledger: decode signing key: %w", err)
		}
		priv = ed25519.PrivateKey(privDec)
		pub = priv.Public().(ed25519.PublicKey)
	}

	return &Ledger{dir: dir, priv: priv, pub: pub}, nil
}

// OpenReadOnly loads the public key only; Append is not available.
func OpenReadOnly(dir string) (*Ledger, error) {
	pubPath := filepath.Join(dir, pubKeyFile)
	pubBytes, err := os.ReadFile(pubPath)
	if err != nil {
		return nil, fmt.Errorf("ledger: read public key: %w", err)
	}
	pubDec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(pubBytes)))
	if err != nil {
		return nil, fmt.Errorf("ledger: decode public key: %w", err)
	}
	return &Ledger{dir: dir, pub: ed25519.PublicKey(pubDec)}, nil
}

// Append signs e and appends it to dir/<taskID>.jsonl. The entry's Ts and
// TaskID fields are filled from the call context if zero/empty.
func (l *Ledger) Append(taskID string, e Entry) error {
	if l.priv == nil {
		return errors.New("ledger: opened read-only")
	}
	if e.Ts.IsZero() {
		e.Ts = time.Now().UTC()
	}
	if e.TaskID == "" {
		e.TaskID = taskID
	}

	// Sign the payload without the Sig field.
	e.Sig = ""
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("ledger: marshal: %w", err)
	}
	sig := ed25519.Sign(l.priv, payload)
	e.Sig = base64.StdEncoding.EncodeToString(sig)

	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("ledger: marshal signed: %w", err)
	}

	path := l.taskPath(taskID)
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return fmt.Errorf("ledger: mkdir: %w", err)
	}
	_, statErr := os.Stat(path)
	firstWrite := errors.Is(statErr, os.ErrNotExist)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("ledger: open %s: %w", path, err)
	}
	if _, werr := f.Write(append(line, '\n')); werr != nil {
		_ = f.Close()
		return werr
	}
	// fsync the file so the appended entry survives a crash before the
	// page cache flushes. Without this, a power loss between write and
	// kernel writeback silently truncates the ledger tail.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("ledger: fsync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("ledger: close %s: %w", path, err)
	}
	// On first create the new dirent also needs to reach disk. Skip on
	// append-to-existing for cost; the file's inode is already durable.
	if firstWrite {
		if dirF, derr := os.Open(l.dir); derr == nil {
			_ = dirF.Sync()
			_ = dirF.Close()
		}
	}
	return nil
}

// ErrBadSignature is returned by Read when an entry's signature does not verify.
var ErrBadSignature = errors.New("ledger: signature verification failed")

// Read returns all entries for taskID, verifying each signature. An entry
// with a bad or missing signature causes an [ErrBadSignature] error that
// names the 1-based line number.
func (l *Ledger) Read(taskID string) ([]Entry, error) {
	path := l.taskPath(taskID)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("ledger: open %s: %w", path, err)
	}
	defer f.Close()

	var entries []Entry
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			return nil, fmt.Errorf("ledger: %s line %d: parse: %w", taskID, line, err)
		}
		if err := l.verify(e, raw); err != nil {
			return nil, fmt.Errorf("%w: %s line %d", ErrBadSignature, taskID, line)
		}
		entries = append(entries, e)
	}
	return entries, sc.Err()
}

// ReadByTenant scans every task file under the ledger directory and returns
// the union of entries whose TenantID matches the supplied id. Entries with
// bad signatures cause the scan to abort with [ErrBadSignature] so callers
// cannot silently miss billing data behind a corrupt file. Pass an empty
// tenantID to select operator-only entries (TenantID == "").
//
// Cost is O(total entries across all tasks) — fine at fleet scale where the
// ledger turnover is small and per-tenant queries run once per billing
// reconciliation, not per request.
func (l *Ledger) ReadByTenant(tenantID string) ([]Entry, error) {
	ids, err := l.Tasks()
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, id := range ids {
		entries, err := l.Read(id)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.TenantID == tenantID {
				out = append(out, e)
			}
		}
	}
	return out, nil
}

// Tasks returns the task IDs that have ledger files, sorted alphabetically.
func (l *Ledger) Tasks() ([]string, error) {
	entries, err := os.ReadDir(l.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ledger: readdir: %w", err)
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			ids = append(ids, strings.TrimSuffix(e.Name(), ".jsonl"))
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// VerifyResult holds the per-task outcome of [Ledger.VerifyAll].
type VerifyResult struct {
	TaskID string
	Count  int
	Err    error
}

// VerifyAll reads every task file, verifying signatures, and returns one
// VerifyResult per task. Tasks with bad signatures have a non-nil Err.
func (l *Ledger) VerifyAll() ([]VerifyResult, error) {
	ids, err := l.Tasks()
	if err != nil {
		return nil, err
	}
	var results []VerifyResult
	for _, id := range ids {
		entries, err := l.Read(id)
		results = append(results, VerifyResult{TaskID: id, Count: len(entries), Err: err})
	}
	return results, nil
}

func (l *Ledger) taskPath(taskID string) string {
	return filepath.Join(l.dir, taskID+".jsonl")
}

// verify re-derives the signed payload from the raw JSON line and checks it
// against the stored public key.
func (l *Ledger) verify(e Entry, _ string) error {
	sig, err := base64.StdEncoding.DecodeString(e.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return ErrBadSignature
	}
	// Reconstruct the payload that was signed (entry without Sig field).
	unsigned := e
	unsigned.Sig = ""
	payload, err := json.Marshal(unsigned)
	if err != nil {
		return err
	}
	if !ed25519.Verify(l.pub, payload, sig) {
		return ErrBadSignature
	}
	return nil
}
