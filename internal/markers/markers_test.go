package markers_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/owera/owera-fleet/internal/markers"
)

func TestWriteAndRead(t *testing.T) {
	s, err := markers.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	m := markers.Marker{
		Pipeline:   "etl-nightly",
		Step:       "extract",
		InputHash:  "sha256:abc",
		OutputPath: "/tmp/out.parquet",
		DurationMs: 1234,
	}
	if err := s.Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := s.Read("etl-nightly", "extract")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.InputHash != "sha256:abc" {
		t.Errorf("InputHash = %q", got.InputHash)
	}
	if got.CompletedAt.IsZero() {
		t.Error("CompletedAt should be filled")
	}
}

func TestIsFresh(t *testing.T) {
	s, _ := markers.NewStore(t.TempDir())
	_ = s.Write(markers.Marker{Pipeline: "p", Step: "s", InputHash: "h1"})

	fresh, err := s.IsFresh("p", "s", "h1")
	if err != nil {
		t.Fatalf("IsFresh: %v", err)
	}
	if !fresh {
		t.Error("should be fresh for matching hash")
	}
	stale, _ := s.IsFresh("p", "s", "h2")
	if stale {
		t.Error("should be stale for different hash")
	}
	missing, _ := s.IsFresh("p", "missing", "any")
	if missing {
		t.Error("missing marker should not be fresh")
	}
}

func TestInvalidate(t *testing.T) {
	s, _ := markers.NewStore(t.TempDir())
	_ = s.Write(markers.Marker{Pipeline: "p", Step: "s", InputHash: "h"})
	if err := s.Invalidate("p", "s"); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	_, err := s.Read("p", "s")
	if !errors.Is(err, markers.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
	// Idempotent.
	if err := s.Invalidate("p", "s"); err != nil {
		t.Errorf("Invalidate should be idempotent: %v", err)
	}
}

func TestListAndPipelines(t *testing.T) {
	s, _ := markers.NewStore(t.TempDir())
	_ = s.Write(markers.Marker{Pipeline: "p1", Step: "b", InputHash: "x"})
	_ = s.Write(markers.Marker{Pipeline: "p1", Step: "a", InputHash: "x"})
	_ = s.Write(markers.Marker{Pipeline: "p2", Step: "z", InputHash: "x"})

	list, err := s.List("p1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d", len(list))
	}
	if list[0].Step != "a" {
		t.Errorf("not sorted: first = %q", list[0].Step)
	}

	pipes, err := s.Pipelines()
	if err != nil {
		t.Fatalf("Pipelines: %v", err)
	}
	if len(pipes) != 2 {
		t.Errorf("expected 2 pipelines, got %v", pipes)
	}
}

func TestHashInput(t *testing.T) {
	hStr, err := markers.HashInput("hello")
	if err != nil {
		t.Fatalf("HashInput string: %v", err)
	}
	if !strings.HasPrefix(hStr, "sha256:") {
		t.Errorf("hash = %q", hStr)
	}
	hBytes, _ := markers.HashInput([]byte("hello"))
	if hStr != hBytes {
		t.Errorf("string and []byte should hash identically: %q vs %q", hStr, hBytes)
	}
	_, err = markers.HashInput(42)
	if err == nil {
		t.Error("expected error for unsupported type")
	}
}

func TestEmptyPipelineOrStep(t *testing.T) {
	s, _ := markers.NewStore(t.TempDir())
	if err := s.Write(markers.Marker{Step: "s", InputHash: "h"}); err == nil {
		t.Error("expected error for empty pipeline")
	}
	if err := s.Write(markers.Marker{Pipeline: "p", InputHash: "h"}); err == nil {
		t.Error("expected error for empty step")
	}
}
