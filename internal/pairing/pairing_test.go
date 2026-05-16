package pairing_test

import (
	"errors"
	"testing"

	"github.com/owera/owera-fleet/internal/pairing"
)

func TestCreateAndGet(t *testing.T) {
	s, err := pairing.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	p, err := s.Create("test-label")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.Token == "" {
		t.Error("Token should be set")
	}
	if !p.Active {
		t.Error("should be active")
	}
	if p.Label != "test-label" {
		t.Errorf("Label = %q", p.Label)
	}

	got, err := s.Get(p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != p.ID {
		t.Errorf("ID mismatch")
	}
}

func TestRevoke(t *testing.T) {
	s, _ := pairing.NewStore(t.TempDir())
	p, _ := s.Create("revoke-me")

	if err := s.Revoke(p.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	got, _ := s.Get(p.ID)
	if got.Active {
		t.Error("should be inactive after revoke")
	}
	if got.RevokedAt.IsZero() {
		t.Error("RevokedAt should be set")
	}
	// Re-revoking is idempotent.
	if err := s.Revoke(p.ID); err != nil {
		t.Fatalf("second Revoke: %v", err)
	}
}

func TestRevokeNonExistent(t *testing.T) {
	s, _ := pairing.NewStore(t.TempDir())
	if err := s.Revoke("does-not-exist"); err != nil {
		t.Errorf("Revoke non-existent should be no-op, got: %v", err)
	}
}

func TestList(t *testing.T) {
	s, _ := pairing.NewStore(t.TempDir())
	_, _ = s.Create("a")
	_, _ = s.Create("b")
	_, _ = s.Create("c")

	all, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3, got %d", len(all))
	}
}

func TestAuthenticate(t *testing.T) {
	s, _ := pairing.NewStore(t.TempDir())
	p, _ := s.Create("auth-test")

	found, err := s.Authenticate(p.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if found.ID != p.ID {
		t.Errorf("got wrong pairing")
	}

	// Bad token.
	_, err = s.Authenticate("bad-token")
	if !errors.Is(err, pairing.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}

	// Revoked token.
	_ = s.Revoke(p.ID)
	_, err = s.Authenticate(p.Token)
	if !errors.Is(err, pairing.ErrNotFound) {
		t.Errorf("revoked token should not authenticate, got %v", err)
	}
}
