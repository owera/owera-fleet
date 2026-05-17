// Package audit implements live-state audits of the Hermes deployment.
//
// DetectDrift compares the live ~/.hermes/config.yaml against the
// documented Hermes-recommended baseline and returns the rows that
// constitute the "Drift from documented baseline" table in
// hermes-setup/SECURITY_NOTES.md.
//
// The baseline encoded here intentionally tracks ONLY the rows that
// SECURITY_NOTES.md's drift table actually lists. Adding more probes
// belongs in a follow-up: the contract for this skill is that running
// `fleetctl audit config` against today's live config reproduces today's
// drift table byte-for-byte (modulo timestamps).
package audit

import (
	"context"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// DriftRow is one row of the SECURITY_NOTES.md drift-from-baseline table.
//
// Setting     — the config.yaml key path being audited.
// Recommended — the value the Hermes security docs recommend.
// Live        — what the live config.yaml actually has.
// Disposition — the "What 'live' actually means in practice" cell from
//                the SECURITY_NOTES.md table: a short, operator-facing
//                description of the consequence of the drift.
type DriftRow struct {
	Setting     string `json:"setting"`
	Recommended string `json:"recommended"`
	Live        string `json:"live"`
	Disposition string `json:"disposition"`
}

// documentedBaseline encodes the Hermes-recommended values that
// SECURITY_NOTES.md's drift table reports against. Keys here MUST match
// SECURITY_NOTES.md's drift table exactly; do not add new probes without
// also adding the corresponding row to SECURITY_NOTES.md.
//
// The value is the "Recommended" string as it appears in the table.
var documentedBaseline = map[string]string{
	"security.website_blocklist.enabled": "true",
	"checkpoints.enabled":                "true",
	"sessions.retention_days":            "30",
	"gateway.allow_all_users/dm_pairing": "set",
}

// dispositions are the "What 'live' actually means in practice" strings
// from the SECURITY_NOTES.md drift table. Indexed by Setting; only
// populated for the live values that trigger a row in the table.
var dispositions = map[string]string{
	"security.website_blocklist.enabled": "No domain-level egress controls — agent can reach any URL",
	"checkpoints.enabled":                "No mid-session rollback snapshots — a misbehaving agent run cannot be reverted",
	"sessions.retention_days":            "Session data kept 3× longer than recommended",
	"gateway.allow_all_users/dm_pairing": "No `gateway:` section; DM-pairing not enforced at config — relies on platform layer",
}

// liveConfig is the subset of ~/.hermes/config.yaml that the drift
// detector cares about. yaml.v3 leaves the absent `gateway:` section
// nil-zero, which is exactly the signal SECURITY_NOTES.md's drift table
// reports.
type liveConfig struct {
	Security struct {
		WebsiteBlocklist struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"website_blocklist"`
	} `yaml:"security"`
	Checkpoints struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"checkpoints"`
	Sessions struct {
		RetentionDays int `yaml:"retention_days"`
	} `yaml:"sessions"`
	Gateway *struct {
		AllowAllUsers *bool `yaml:"allow_all_users,omitempty"`
		DMPairing     *bool `yaml:"dm_pairing,omitempty"`
	} `yaml:"gateway,omitempty"`
}

// DetectDrift reads configPath as YAML and returns the drift rows that
// match SECURITY_NOTES.md's "Drift from documented baseline" table.
//
// The returned slice preserves the row order used in SECURITY_NOTES.md
// so callers (markdown / JSON renderers) can emit it unchanged.
//
// ctx is currently unused but is part of the public signature for
// symmetry with the rest of internal/* (every Detect-style API there
// takes a ctx so the audit subcommand can wire cancellation later).
func DetectDrift(ctx context.Context, configPath string) ([]DriftRow, error) {
	_ = ctx

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("audit: read %s: %w", configPath, err)
	}
	var cfg liveConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("audit: parse %s: %w", configPath, err)
	}

	// Order here mirrors the SECURITY_NOTES.md drift table.
	var rows []DriftRow

	// security.website_blocklist.enabled — recommended true, drift if false.
	if !cfg.Security.WebsiteBlocklist.Enabled {
		rows = append(rows, DriftRow{
			Setting:     "security.website_blocklist.enabled",
			Recommended: documentedBaseline["security.website_blocklist.enabled"],
			Live:        "false",
			Disposition: dispositions["security.website_blocklist.enabled"],
		})
	}

	// checkpoints.enabled — recommended true, drift if false.
	if !cfg.Checkpoints.Enabled {
		rows = append(rows, DriftRow{
			Setting:     "checkpoints.enabled",
			Recommended: documentedBaseline["checkpoints.enabled"],
			Live:        "false",
			Disposition: dispositions["checkpoints.enabled"],
		})
	}

	// sessions.retention_days — recommended 30, drift if > 30.
	if cfg.Sessions.RetentionDays > 30 {
		rows = append(rows, DriftRow{
			Setting:     "sessions.retention_days",
			Recommended: documentedBaseline["sessions.retention_days"],
			Live:        fmt.Sprintf("%d", cfg.Sessions.RetentionDays),
			Disposition: dispositions["sessions.retention_days"],
		})
	}

	// gateway.allow_all_users / dm_pairing — recommended set, drift if absent.
	// Absent = whole `gateway:` section missing OR both keys nil.
	if cfg.Gateway == nil || (cfg.Gateway.AllowAllUsers == nil && cfg.Gateway.DMPairing == nil) {
		rows = append(rows, DriftRow{
			Setting:     "gateway.allow_all_users/dm_pairing",
			Recommended: documentedBaseline["gateway.allow_all_users/dm_pairing"],
			Live:        "absent",
			Disposition: dispositions["gateway.allow_all_users/dm_pairing"],
		})
	}

	return rows, nil
}
