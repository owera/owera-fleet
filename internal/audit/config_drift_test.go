package audit_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/owera/owera-fleet/internal/audit"
)

// zeroDriftFixture: every audited setting matches the recommended baseline.
func TestDetectDrift_ZeroDrift(t *testing.T) {
	rows, err := audit.DetectDrift(context.Background(), filepath.Join("testdata", "config_zero_drift.yaml"))
	if err != nil {
		t.Fatalf("DetectDrift: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 drift rows for clean config, got %d: %+v", len(rows), rows)
	}
}

// oneDriftFixture: only sessions.retention_days is out of baseline.
func TestDetectDrift_OneDrift(t *testing.T) {
	rows, err := audit.DetectDrift(context.Background(), filepath.Join("testdata", "config_one_drift.yaml"))
	if err != nil {
		t.Fatalf("DetectDrift: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 drift row, got %d: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.Setting != "sessions.retention_days" {
		t.Errorf("Setting = %q, want sessions.retention_days", got.Setting)
	}
	if got.Recommended != "30" {
		t.Errorf("Recommended = %q, want 30", got.Recommended)
	}
	if got.Live != "90" {
		t.Errorf("Live = %q, want 90", got.Live)
	}
	if got.Disposition == "" {
		t.Errorf("Disposition is empty; expected SECURITY_NOTES.md prose")
	}
}

// multiDriftFixture: all four SECURITY_NOTES.md drift rows fire.
// Order must match the SECURITY_NOTES.md table because the markdown
// renderer emits rows in slice order to match the doc byte-for-byte.
func TestDetectDrift_MultiDrift(t *testing.T) {
	rows, err := audit.DetectDrift(context.Background(), filepath.Join("testdata", "config_multi_drift.yaml"))
	if err != nil {
		t.Fatalf("DetectDrift: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("expected 4 drift rows, got %d: %+v", len(rows), rows)
	}

	wantOrder := []struct {
		setting     string
		recommended string
		live        string
	}{
		{"security.website_blocklist.enabled", "true", "false"},
		{"checkpoints.enabled", "true", "false"},
		{"sessions.retention_days", "30", "90"},
		{"gateway.allow_all_users/dm_pairing", "set", "absent"},
	}
	for i, want := range wantOrder {
		got := rows[i]
		if got.Setting != want.setting {
			t.Errorf("row[%d].Setting = %q, want %q", i, got.Setting, want.setting)
		}
		if got.Recommended != want.recommended {
			t.Errorf("row[%d].Recommended = %q, want %q", i, got.Recommended, want.recommended)
		}
		if got.Live != want.live {
			t.Errorf("row[%d].Live = %q, want %q", i, got.Live, want.live)
		}
		if got.Disposition == "" {
			t.Errorf("row[%d].Disposition is empty", i)
		}
	}
}

// Missing config file is a hard error — the audit subcommand exits
// non-zero rather than reporting "no drift".
func TestDetectDrift_MissingFile(t *testing.T) {
	_, err := audit.DetectDrift(context.Background(), filepath.Join("testdata", "does_not_exist.yaml"))
	if err == nil {
		t.Fatalf("expected error for missing config, got nil")
	}
}
