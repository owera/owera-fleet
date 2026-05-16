// Package nodes parses the worker registry at ~/.hermes/nodes.txt and
// exposes the canonical iteration helpers (All / One / Random) that every
// fleet command uses to dispatch work.
//
// The file format is intentionally minimal: one "user@host" per line, with
// blank lines and lines starting with "#" treated as comments. This matches
// the format established by hermes-setup and is sufficient for the actual
// fleet size (≤10 workers). Tagged routing belongs in a future skill rather
// than this parser.
package nodes

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
)

// DefaultPath is the canonical registry location. Resolution is lazy so
// tests can override HOME without import-time side effects.
const DefaultPath = "~/.hermes/nodes.txt"

// Node is one entry in the registry.
type Node struct {
	User string `json:"user"`
	Host string `json:"host"`
}

// String returns the canonical "user@host" form.
func (n Node) String() string { return n.User + "@" + n.Host }

// Registry is an ordered collection of nodes with iteration helpers.
//
// Order matches the file's line order, which lets operators control
// dispatch priority by reordering nodes.txt without other registry changes.
type Registry struct {
	nodes []Node
	rnd   *rand.Rand
}

// New returns a Registry over the given nodes. The slice is copied so
// callers can mutate their original without affecting the registry.
func New(nodes []Node) *Registry {
	r := &Registry{nodes: append([]Node(nil), nodes...)}
	r.rnd = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	return r
}

// Load reads and parses path. If path is "" or starts with "~/", it expands
// against the operator's home directory.
func Load(path string) (*Registry, error) {
	if path == "" {
		path = DefaultPath
	}
	resolved, err := expandHome(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, fmt.Errorf("nodes: open %s: %w", resolved, err)
	}
	defer f.Close()
	return Parse(f)
}

// Parse reads "user@host" entries from r, skipping comments and blanks.
//
// Malformed lines (no "@", empty user, empty host) return an error tagged
// with the 1-based line number so operators can fix nodes.txt in place.
func Parse(r io.Reader) (*Registry, error) {
	var out []Node
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), 1<<20)

	lineNo := 0
	for s.Scan() {
		lineNo++
		raw := strings.TrimSpace(s.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		// Strip a trailing comment if present so operators can annotate entries.
		if i := strings.Index(raw, " #"); i >= 0 {
			raw = strings.TrimSpace(raw[:i])
		}
		at := strings.IndexByte(raw, '@')
		if at <= 0 || at == len(raw)-1 {
			return nil, fmt.Errorf("nodes: line %d: malformed entry %q (expected user@host)", lineNo, raw)
		}
		user := strings.TrimSpace(raw[:at])
		host := strings.TrimSpace(raw[at+1:])
		if user == "" || host == "" {
			return nil, fmt.Errorf("nodes: line %d: empty user or host in %q", lineNo, raw)
		}
		out = append(out, Node{User: user, Host: host})
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("nodes: scan: %w", err)
	}
	return New(out), nil
}

// Len returns the number of registered nodes.
func (r *Registry) Len() int { return len(r.nodes) }

// All returns a copy of the registered nodes in file order.
func (r *Registry) All() []Node {
	return append([]Node(nil), r.nodes...)
}

// ErrNotFound is returned by One when no node matches the requested name.
var ErrNotFound = errors.New("nodes: not found")

// ErrEmpty is returned by Random when the registry has no nodes.
var ErrEmpty = errors.New("nodes: registry is empty")

// One returns the node matching name, which may be either the full
// "user@host" form or just "host" (in which case the first node with that
// host wins — operator-friendly when the user is implicit).
func (r *Registry) One(name string) (Node, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Node{}, ErrNotFound
	}
	for _, n := range r.nodes {
		if n.String() == name {
			return n, nil
		}
	}
	for _, n := range r.nodes {
		if n.Host == name {
			return n, nil
		}
	}
	return Node{}, fmt.Errorf("%w: %q", ErrNotFound, name)
}

// Random returns one node chosen uniformly at random.
func (r *Registry) Random() (Node, error) {
	if len(r.nodes) == 0 {
		return Node{}, ErrEmpty
	}
	return r.nodes[r.rnd.IntN(len(r.nodes))], nil
}

// SetSource overrides the random source. Intended for deterministic tests.
func (r *Registry) SetSource(src *rand.Rand) { r.rnd = src }

func expandHome(path string) (string, error) {
	if !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("nodes: resolve home: %w", err)
	}
	return filepath.Join(home, path[2:]), nil
}
