// Package json provides escape and dotted-path "pluck" helpers for the
// places in owera-fleet that need to read or assemble JSON without
// declaring a Go struct — mostly: poking at JSONL log lines, scenario YAML
// inputs that get passed through to remote/, and one-off audit queries.
//
// For routine encoding/decoding into typed structs, prefer encoding/json
// from the stdlib. This package is for the cases where the schema is
// loose, dynamic, or where building a one-line jq-like accessor in Go is
// simpler than allocating a map[string]any tree.
package json

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Escape returns s wrapped as a JSON-encoded string (including the
// surrounding quotes). Special characters are escaped per RFC 8259.
//
// Use this when you're emitting JSON by hand (e.g., from a bash-shaped
// template) and need a single field to be safe. For full objects, use
// encoding/json.Marshal.
func Escape(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ErrNotFound is returned by the typed Pluck* helpers when the requested
// path does not exist in the document.
var ErrNotFound = errors.New("json: path not found")

// ErrWrongType is returned by the typed Pluck* helpers when the path
// resolves to a value of an unexpected type.
var ErrWrongType = errors.New("json: wrong type at path")

// Pluck extracts the value at a dotted path from a JSON document.
//
// Path syntax:
//   - "" returns the whole decoded document.
//   - "foo.bar" descends into nested objects.
//   - "items.0" indexes into arrays (0-based).
//   - "foo.bar.0.name" mixes both.
//
// The returned value is whatever encoding/json decoded for that node —
// typically string, float64, bool, nil, map[string]any, or []any.
// found is false when any segment is missing; err is non-nil only on
// malformed JSON or a non-traversable intermediate (e.g., descending into
// a string).
func Pluck(doc []byte, path string) (val any, found bool, err error) {
	var root any
	if err := json.Unmarshal(doc, &root); err != nil {
		return nil, false, fmt.Errorf("json: unmarshal: %w", err)
	}
	if path == "" {
		return root, true, nil
	}
	cur := root
	for _, seg := range strings.Split(path, ".") {
		switch v := cur.(type) {
		case map[string]any:
			next, ok := v[seg]
			if !ok {
				return nil, false, nil
			}
			cur = next
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil {
				return nil, false, fmt.Errorf("json: array segment %q is not an index", seg)
			}
			if idx < 0 || idx >= len(v) {
				return nil, false, nil
			}
			cur = v[idx]
		default:
			return nil, false, fmt.Errorf("json: segment %q traversed into %T", seg, cur)
		}
	}
	return cur, true, nil
}

// PluckString returns the string at path or an error.
//
// Numbers and bools are not coerced to strings — that's a type mismatch and
// returns ErrWrongType. Use Pluck if you want loose typing.
func PluckString(doc []byte, path string) (string, error) {
	v, ok, err := Pluck(doc, path)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrNotFound, path)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%w: %q is %T", ErrWrongType, path, v)
	}
	return s, nil
}

// PluckInt returns the integer at path or an error.
//
// Floats with a non-zero fractional part are rejected as ErrWrongType so
// callers don't accept rounded data by accident.
func PluckInt(doc []byte, path string) (int64, error) {
	v, ok, err := Pluck(doc, path)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrNotFound, path)
	}
	f, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("%w: %q is %T", ErrWrongType, path, v)
	}
	if f != float64(int64(f)) {
		return 0, fmt.Errorf("%w: %q is fractional (%v)", ErrWrongType, path, f)
	}
	return int64(f), nil
}

// PluckBool returns the bool at path or an error.
func PluckBool(doc []byte, path string) (bool, error) {
	v, ok, err := Pluck(doc, path)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("%w: %q", ErrNotFound, path)
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("%w: %q is %T", ErrWrongType, path, v)
	}
	return b, nil
}
