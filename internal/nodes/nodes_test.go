package nodes

import (
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const liveFleet = `# Owera fleet registry
# format: user@host (one per line)
hermes@claw1.local
hermes@claw2.local  # arm64 worker
`

func parse(t *testing.T, s string) *Registry {
	t.Helper()
	r, err := Parse(strings.NewReader(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return r
}

func TestParse_LiveFleetShape(t *testing.T) {
	r := parse(t, liveFleet)
	if got, want := r.Len(), 2; got != want {
		t.Fatalf("Len = %d, want %d", got, want)
	}
	all := r.All()
	if all[0].String() != "hermes@claw1.local" || all[1].String() != "hermes@claw2.local" {
		t.Errorf("unexpected order: %v", all)
	}
}

func TestParse_SkipsBlanksAndComments(t *testing.T) {
	in := "\n# header\n\nhermes@a.local\n  \n\t# indented comment\nhermes@b.local\n"
	r := parse(t, in)
	if r.Len() != 2 {
		t.Fatalf("Len = %d, want 2", r.Len())
	}
}

func TestParse_TrailingComment(t *testing.T) {
	r := parse(t, "hermes@worker.local # production node\n")
	if got, want := r.All()[0].String(), "hermes@worker.local"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParse_Malformed(t *testing.T) {
	cases := []string{
		"no-at-sign-here\n",
		"@only-host\n",
		"only-user@\n",
		" @ \n",
	}
	for _, in := range cases {
		_, err := Parse(strings.NewReader(in))
		if err == nil {
			t.Errorf("expected error for %q, got nil", in)
		}
		if err != nil && !strings.Contains(err.Error(), "line 1") {
			t.Errorf("error for %q should reference line 1: %v", in, err)
		}
	}
}

func TestParse_ErrorReportsCorrectLine(t *testing.T) {
	in := "hermes@a.local\n# comment\nbroken\n"
	_, err := Parse(strings.NewReader(in))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "line 3") {
		t.Errorf("error should reference line 3, got %v", err)
	}
}

func TestOne_ExactMatch(t *testing.T) {
	r := parse(t, liveFleet)
	got, err := r.One("hermes@claw2.local")
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if got.Host != "claw2.local" || got.User != "hermes" {
		t.Errorf("got %+v", got)
	}
}

func TestOne_HostOnly(t *testing.T) {
	r := parse(t, liveFleet)
	got, err := r.One("claw1.local")
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if got.String() != "hermes@claw1.local" {
		t.Errorf("got %q", got)
	}
}

func TestOne_NotFound(t *testing.T) {
	r := parse(t, liveFleet)
	_, err := r.One("nobody@somewhere.local")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestOne_EmptyName(t *testing.T) {
	r := parse(t, liveFleet)
	if _, err := r.One(""); !errors.Is(err, ErrNotFound) {
		t.Errorf("empty name: got %v, want ErrNotFound", err)
	}
}

func TestRandom_DeterministicWithSeed(t *testing.T) {
	r := parse(t, liveFleet)
	r.SetSource(rand.New(rand.NewPCG(1, 2)))
	first, err := r.Random()
	if err != nil {
		t.Fatalf("Random: %v", err)
	}
	second, err := r.Random()
	if err != nil {
		t.Fatalf("Random: %v", err)
	}
	// Both picks must be from the registry.
	if first.User != "hermes" || second.User != "hermes" {
		t.Errorf("Random returned non-registry node(s): %v %v", first, second)
	}
}

func TestRandom_EmptyRegistry(t *testing.T) {
	r := parse(t, "# only comments\n")
	if _, err := r.Random(); !errors.Is(err, ErrEmpty) {
		t.Errorf("got %v, want ErrEmpty", err)
	}
}

func TestRandom_CoversAllOverManyTrials(t *testing.T) {
	r := parse(t, liveFleet)
	r.SetSource(rand.New(rand.NewPCG(42, 43)))
	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		n, err := r.Random()
		if err != nil {
			t.Fatalf("Random: %v", err)
		}
		seen[n.String()]++
	}
	if len(seen) != 2 {
		t.Errorf("expected to see both nodes over 200 trials, got %v", seen)
	}
}

func TestLoad_FromFile(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "nodes.txt")
	if err := os.WriteFile(path, []byte(liveFleet), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	r, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Len() != 2 {
		t.Errorf("Len = %d, want 2", r.Len())
	}
}

func TestLoad_HomeExpansion(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	dir := filepath.Join(tmp, ".hermes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nodes.txt"), []byte("hermes@only.local\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	r, err := Load("") // empty → DefaultPath
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Len() != 1 || r.All()[0].Host != "only.local" {
		t.Errorf("unexpected registry: %v", r.All())
	}
}

func TestLoad_Missing(t *testing.T) {
	_, err := Load("/definitely/not/a/real/path/nodes.txt")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestAll_IsCopy(t *testing.T) {
	r := parse(t, liveFleet)
	a := r.All()
	a[0] = Node{User: "x", Host: "y"}
	if r.All()[0].String() != "hermes@claw1.local" {
		t.Errorf("All() returned a shared slice: %v", r.All())
	}
}
