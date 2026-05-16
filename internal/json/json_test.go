package json

import (
	"errors"
	"strings"
	"testing"
)

var sampleEvent = []byte(`{
  "ts": "2026-05-16T15:30:00Z",
  "node": "hermes@claw1.local",
  "phase": "delegate",
  "action": "run-command",
  "result": "ok",
  "duration_ms": 142,
  "stderr_tail": "",
  "tags": ["audit", "fast"],
  "meta": {"build": 12, "ok": true}
}`)

func TestEscape_RoundTrip(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"plain", `"plain"`},
		{`needs "quotes"`, `"needs \"quotes\""`},
		{"new\nline", `"new\nline"`},
		{"tab\there", `"tab\there"`},
		{"", `""`},
	}
	for _, c := range cases {
		if got := Escape(c.in); got != c.want {
			t.Errorf("Escape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPluck_TopLevel(t *testing.T) {
	v, ok, err := Pluck(sampleEvent, "node")
	if err != nil || !ok {
		t.Fatalf("Pluck node: err=%v ok=%v", err, ok)
	}
	if v.(string) != "hermes@claw1.local" {
		t.Errorf("got %v", v)
	}
}

func TestPluck_EmptyPath(t *testing.T) {
	v, ok, err := Pluck(sampleEvent, "")
	if err != nil || !ok {
		t.Fatalf("Pluck root: err=%v ok=%v", err, ok)
	}
	if _, isMap := v.(map[string]any); !isMap {
		t.Errorf("root not a map: %T", v)
	}
}

func TestPluck_NestedObject(t *testing.T) {
	v, ok, err := Pluck(sampleEvent, "meta.build")
	if err != nil || !ok {
		t.Fatalf("Pluck meta.build: err=%v ok=%v", err, ok)
	}
	if v.(float64) != 12 {
		t.Errorf("got %v", v)
	}
}

func TestPluck_ArrayIndex(t *testing.T) {
	v, ok, err := Pluck(sampleEvent, "tags.0")
	if err != nil || !ok {
		t.Fatalf("Pluck tags.0: err=%v ok=%v", err, ok)
	}
	if v.(string) != "audit" {
		t.Errorf("got %v", v)
	}
}

func TestPluck_ArrayOutOfRange(t *testing.T) {
	v, ok, err := Pluck(sampleEvent, "tags.5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok || v != nil {
		t.Errorf("expected not-found, got ok=%v v=%v", ok, v)
	}
}

func TestPluck_MissingKey(t *testing.T) {
	v, ok, err := Pluck(sampleEvent, "meta.missing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok || v != nil {
		t.Errorf("expected not-found, got ok=%v v=%v", ok, v)
	}
}

func TestPluck_TraverseIntoScalar(t *testing.T) {
	_, _, err := Pluck(sampleEvent, "node.something")
	if err == nil {
		t.Fatal("expected error traversing into a string")
	}
}

func TestPluck_MalformedJSON(t *testing.T) {
	_, _, err := Pluck([]byte("{not json"), "x")
	if err == nil {
		t.Fatal("expected unmarshal error")
	}
	if !strings.Contains(err.Error(), "unmarshal") {
		t.Errorf("error should mention unmarshal: %v", err)
	}
}

func TestPluckString_Happy(t *testing.T) {
	s, err := PluckString(sampleEvent, "result")
	if err != nil {
		t.Fatalf("PluckString: %v", err)
	}
	if s != "ok" {
		t.Errorf("got %q", s)
	}
}

func TestPluckString_NotFound(t *testing.T) {
	_, err := PluckString(sampleEvent, "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestPluckString_WrongType(t *testing.T) {
	_, err := PluckString(sampleEvent, "duration_ms")
	if !errors.Is(err, ErrWrongType) {
		t.Errorf("got %v, want ErrWrongType", err)
	}
}

func TestPluckInt_Happy(t *testing.T) {
	n, err := PluckInt(sampleEvent, "duration_ms")
	if err != nil {
		t.Fatalf("PluckInt: %v", err)
	}
	if n != 142 {
		t.Errorf("got %d", n)
	}
}

func TestPluckInt_Fractional(t *testing.T) {
	doc := []byte(`{"value": 1.5}`)
	_, err := PluckInt(doc, "value")
	if !errors.Is(err, ErrWrongType) {
		t.Errorf("got %v, want ErrWrongType for fractional value", err)
	}
}

func TestPluckBool_Happy(t *testing.T) {
	b, err := PluckBool(sampleEvent, "meta.ok")
	if err != nil {
		t.Fatalf("PluckBool: %v", err)
	}
	if !b {
		t.Errorf("got %v", b)
	}
}

func TestPluckBool_WrongType(t *testing.T) {
	_, err := PluckBool(sampleEvent, "result")
	if !errors.Is(err, ErrWrongType) {
		t.Errorf("got %v, want ErrWrongType", err)
	}
}

func TestPluck_NumericKeyOnArray(t *testing.T) {
	doc := []byte(`["a","b","c"]`)
	v, ok, err := Pluck(doc, "1")
	if err != nil || !ok {
		t.Fatalf("Pluck array[1]: err=%v ok=%v", err, ok)
	}
	if v.(string) != "b" {
		t.Errorf("got %v", v)
	}
}

func TestPluck_NonNumericSegmentOnArray(t *testing.T) {
	doc := []byte(`["a","b","c"]`)
	_, _, err := Pluck(doc, "abc")
	if err == nil {
		t.Fatal("expected error for non-numeric segment on array")
	}
}
