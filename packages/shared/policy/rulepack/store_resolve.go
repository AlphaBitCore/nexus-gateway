// Package rulepack: store_resolve.go — install upgrade + effective rule-set resolution.
package rulepack

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// UpgradeResult describes the outcome of UpgradeInstallToLatest.
type UpgradeResult struct {
	InstallID   string `json:"installId"`
	PackName    string `json:"packName"`
	FromVersion string `json:"fromVersion"`
	ToVersion   string `json:"toVersion"`
	// Upgraded is false when the install was already pinned to the latest
	// version (no-op); true when the pin was advanced.
	Upgraded bool `json:"upgraded"`
}

// LatestVersionForPack returns the rule_pack row id and version string of the
// newest version in a pack family (by name), ordered by semver. Returns
// ErrPackNotFound when no version of the family exists. Used by the
// "upgrade install to latest" flow: installs pin to an exact version row via
// packId, so advancing a pin means repointing packId at the latest row.
func (s *Store) LatestVersionForPack(ctx context.Context, name string) (packID, version string, err error) {
	rows, err := s.pool.Query(ctx, `SELECT id, version FROM "rule_pack" WHERE name = $1`, name)
	if err != nil {
		return "", "", fmt.Errorf("rulepack.LatestVersionForPack: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, v string
		if err := rows.Scan(&id, &v); err != nil {
			return "", "", fmt.Errorf("rulepack.LatestVersionForPack: scan: %w", err)
		}
		if version == "" || CompareSemver(v, version) > 0 {
			packID, version = id, v
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", fmt.Errorf("rulepack.LatestVersionForPack: %w", err)
	}
	if packID == "" {
		return "", "", ErrPackNotFound
	}
	return packID, version, nil
}

// UpgradeInstallToLatest repoints an install at the newest version of its pack
// family and syncs the pinVersion label. Installs pin to a specific version
// row through packId, so an "upgrade" is a packId+pinVersion swap; the bound
// hook, enabled flag, and per-rule overrides (keyed by installId, not packId)
// are preserved. Returns Upgraded=false when the install is already on the
// latest version. Returns ErrInstallNotFound for an unknown install.
func (s *Store) UpgradeInstallToLatest(ctx context.Context, installID string) (*UpgradeResult, error) {
	var name, fromVersion string
	err := s.pool.QueryRow(ctx, `
		SELECT p.name, i."pinVersion"
		FROM "rule_pack_install" i
		JOIN "rule_pack" p ON p.id = i."packId"
		WHERE i.id = $1`,
		installID,
	).Scan(&name, &fromVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInstallNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("rulepack.UpgradeInstallToLatest: load install: %w", err)
	}

	latestID, latestVersion, err := s.LatestVersionForPack(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("rulepack.UpgradeInstallToLatest: %w", err)
	}

	res := &UpgradeResult{
		InstallID:   installID,
		PackName:    name,
		FromVersion: fromVersion,
		ToVersion:   latestVersion,
	}
	if CompareSemver(latestVersion, fromVersion) <= 0 {
		// Already at (or somehow ahead of) latest — no-op.
		return res, nil
	}

	tag, err := s.pool.Exec(ctx,
		`UPDATE "rule_pack_install" SET "packId" = $2, "pinVersion" = $3 WHERE id = $1`,
		installID, latestID, latestVersion)
	if err != nil {
		return nil, fmt.Errorf("rulepack.UpgradeInstallToLatest: update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrInstallNotFound
	}
	res.Upgraded = true
	return res, nil
}

// ListInstallsForHook returns every rule_pack_install bound to hookID,
// including the pack name for convenience. Disabled installs are included
// (callers decide whether to evaluate them; the admin UI needs visibility).
// Ordered by installedAt ASC so the rule-pack engine scans in install order.
func (s *Store) ListInstallsForHook(ctx context.Context, hookID string) ([]Install, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, i."packId", p.name, i."pinVersion", i."boundHookId", i.enabled, i."installedAt"
		FROM "rule_pack_install" i
		JOIN "rule_pack" p ON p.id = i."packId"
		WHERE i."boundHookId" = $1
		ORDER BY i."installedAt" ASC`, hookID)
	if err != nil {
		return nil, fmt.Errorf("rulepack.ListInstallsForHook: %w", err)
	}
	defer rows.Close()
	var out []Install
	for rows.Next() {
		var inst Install
		if err := rows.Scan(&inst.ID, &inst.PackID, &inst.PackName, &inst.PinVersion,
			&inst.BoundHookID, &inst.Enabled, &inst.InstalledAt); err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, rows.Err()
}

// LoadEffectiveSetsForHook returns every enabled install bound to hookID,
// each resolved to its post-override EffectiveRuleSet. Disabled installs
// are filtered out — the runtime engine never sees them. This is the
// method the data-plane config loader calls when enriching HookConfig
// with its bound rule packs.
func (s *Store) LoadEffectiveSetsForHook(ctx context.Context, hookID string) ([]EffectiveRuleSet, error) {
	installs, err := s.ListInstallsForHook(ctx, hookID)
	if err != nil {
		return nil, err
	}
	out := make([]EffectiveRuleSet, 0, len(installs))
	for _, inst := range installs {
		if !inst.Enabled {
			continue
		}
		eff, err := s.LoadForInstall(ctx, inst.ID)
		if err != nil {
			return nil, fmt.Errorf("rulepack.LoadEffectiveSetsForHook: install %s: %w", inst.ID, err)
		}
		out = append(out, *eff)
	}
	return out, nil
}

// LoadForInstall returns the post-override rule list for a given install.
// The returned EffectiveRuleSet is cache-friendly — hook factories call
// this at pipeline-build time and hold the reference for the pipeline's
// lifetime.
func (s *Store) LoadForInstall(ctx context.Context, installID string) (*EffectiveRuleSet, error) {
	var inst Install
	err := s.pool.QueryRow(ctx, `
		SELECT i.id, i."packId", p.name, i."pinVersion", i."boundHookId", i.enabled, i."installedAt"
		FROM "rule_pack_install" i
		JOIN "rule_pack" p ON p.id = i."packId"
		WHERE i.id = $1`, installID,
	).Scan(&inst.ID, &inst.PackID, &inst.PackName, &inst.PinVersion, &inst.BoundHookID, &inst.Enabled, &inst.InstalledAt)
	if err != nil {
		return nil, fmt.Errorf("rulepack.LoadForInstall: load install: %w", err)
	}
	pack, err := s.GetPack(ctx, inst.PackID)
	if err != nil {
		return nil, fmt.Errorf("rulepack.LoadForInstall: load pack: %w", err)
	}
	// Load overrides for this install.
	rows, err := s.pool.Query(ctx, `
		SELECT "ruleLocalId", disabled, COALESCE("severityOverride", '')
		FROM "rule_override" WHERE "installId" = $1`, installID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	overrides := map[string]Override{}
	for rows.Next() {
		var o Override
		if err := rows.Scan(&o.RuleLocalID, &o.Disabled, &o.SeverityOverride); err != nil {
			return nil, err
		}
		overrides[o.RuleLocalID] = o
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Apply overrides: filter disabled rules, swap severity.
	effective := make([]Rule, 0, len(pack.Rules))
	for _, r := range pack.Rules {
		if o, ok := overrides[r.RuleID]; ok {
			if o.Disabled {
				continue
			}
			if o.SeverityOverride != "" {
				r.Severity = o.SeverityOverride
			}
		}
		effective = append(effective, r)
	}
	pack.Rules = effective
	return &EffectiveRuleSet{Install: inst, Pack: *pack}, nil
}
