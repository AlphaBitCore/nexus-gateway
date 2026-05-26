package catbagent

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// AgentInstalledRulePacksLoader aggregates the rule_pack_install rows
// (joined with rule_pack for catalog metadata) so the agent's Policies
// page can render the same Rule Packs view CP-UI shows admins. The agent
// itself does not run rule packs as a separate execution unit — the
// constituent rules flow into the hook chain via hook_config — but the
// user-facing read-only view still needs the pack metadata so a user
// can see "admin installed pack X into hook Y on this device".
//
// Shape returned by Load (matches the agent's parseRulePacks):
//
//	{"installedRulePacks": [{
//	    id, packId, name, version, maintainer, description,
//	    boundHookId, enabled, ruleCount, installedAt
//	}]}
//
// Today every agent sees every rule pack install (no device-group
// scoping); the WHERE clause is where to add that filter later.
type AgentInstalledRulePacksLoader struct {
	db     pgxQuerier
	logger *slog.Logger
}

// NewAgentInstalledRulePacksLoader constructs a loader bound to the
// given pool. The logger is optional.
func NewAgentInstalledRulePacksLoader(db pgxQuerier, logger *slog.Logger) *AgentInstalledRulePacksLoader {
	return &AgentInstalledRulePacksLoader{db: db, logger: logger}
}

// agentInstalledRulePackRow mirrors the rule_pack_install JOIN rule_pack
// projection. Field tags carry the wire-shape names the agent's
// policies.parseRulePacks expects; any rename here must be mirrored
// there.
type agentInstalledRulePackRow struct {
	ID          string `json:"id"`
	PackID      string `json:"packId"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Maintainer  string `json:"maintainer,omitempty"`
	Description string `json:"description,omitempty"`
	BoundHookID string `json:"boundHookId"`
	Enabled     bool   `json:"enabled"`
	RuleCount   int    `json:"ruleCount"`
	InstalledAt string `json:"installedAt,omitempty"`
	// Rules are unconditional base data — every install ships the full
	// rule list so the user-facing Dashboard can show pattern / category /
	// severity for each entry without a follow-up Hub round-trip.
	Rules []agentPackRuleRow `json:"rules"`
}

// agentPackRuleRow mirrors a single rule row inside a rule pack. Used
// only by the Cat B loader; the agent-side mirror is policies.RulePackRule.
type agentPackRuleRow struct {
	ID          string   `json:"id"`
	RuleID      string   `json:"ruleId"`
	Category    string   `json:"category"`
	Severity    string   `json:"severity"`
	Pattern     string   `json:"pattern"`
	Flags       string   `json:"flags,omitempty"`
	Description string   `json:"description,omitempty"`
	Labels      []string `json:"labels,omitempty"`
}

// Load returns every rule_pack_install joined with rule_pack catalog
// metadata. For each pack the full rule list is loaded too (rules are
// unconditional base data). Version is derived from MAX(installedAt)
// across installs — there is no updatedAt column on rule_pack_install
// so this is the coarsest monotonic signal we can produce.
func (l *AgentInstalledRulePacksLoader) Load(ctx context.Context, _ string) (any, int64, error) {
	rows, err := l.db.Query(ctx, `
		SELECT
			rpi.id,
			rpi."packId",
			rp.name,
			rpi."pinVersion",
			rp.maintainer,
			COALESCE(rp.description, ''),
			rpi."boundHookId",
			rpi.enabled,
			rpi."installedAt"
		FROM rule_pack_install rpi
		JOIN rule_pack rp ON rp.id = rpi."packId"
		ORDER BY rpi."installedAt" DESC
	`)
	if err != nil {
		return nil, 0, fmt.Errorf("catb: query rule_pack_install: %w", err)
	}
	defer rows.Close()

	var (
		packs      []agentInstalledRulePackRow
		packsByID  = make(map[string]int)
		maxUpdated time.Time
		packIDs    []string
	)
	for rows.Next() {
		var (
			p           agentInstalledRulePackRow
			installedAt time.Time
		)
		if err := rows.Scan(
			&p.ID, &p.PackID, &p.Name, &p.Version, &p.Maintainer, &p.Description,
			&p.BoundHookID, &p.Enabled, &installedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("catb: scan rule_pack_install: %w", err)
		}
		p.InstalledAt = installedAt.UTC().Format(time.RFC3339)
		p.Rules = []agentPackRuleRow{}
		packsByID[p.PackID] = len(packs)
		packs = append(packs, p)
		packIDs = append(packIDs, p.PackID)
		if installedAt.After(maxUpdated) {
			maxUpdated = installedAt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("catb: iterate rule_pack_install: %w", err)
	}

	if packs == nil {
		packs = []agentInstalledRulePackRow{}
	}

	// Load rules for every unique pack referenced by an install. One
	// query keyed on packId IN (...) so we don't N+1 the DB.
	if len(packIDs) > 0 {
		ruleRows, err := l.db.Query(ctx, `
			SELECT id, "packId", "ruleId", category, severity, pattern,
			       COALESCE(flags, ''), COALESCE(description, ''), labels
			FROM rule
			WHERE "packId" = ANY($1)
			ORDER BY severity DESC, category ASC, "ruleId" ASC
		`, packIDs)
		if err != nil {
			return nil, 0, fmt.Errorf("catb: query rule: %w", err)
		}
		defer ruleRows.Close()
		for ruleRows.Next() {
			var (
				r      agentPackRuleRow
				packID string
				labels []string
			)
			if err := ruleRows.Scan(&r.ID, &packID, &r.RuleID, &r.Category,
				&r.Severity, &r.Pattern, &r.Flags, &r.Description, &labels); err != nil {
				return nil, 0, fmt.Errorf("catb: scan rule: %w", err)
			}
			r.Labels = labels
			packs[packsByID[packID]].Rules = append(packs[packsByID[packID]].Rules, r)
			packs[packsByID[packID]].RuleCount++
		}
		if err := ruleRows.Err(); err != nil {
			return nil, 0, fmt.Errorf("catb: iterate rule: %w", err)
		}
	}

	// UnixNano returns negative values for timestamps before 1678 — impossible
	// for rule_pack_install rows in any real deployment. Use timestampVersion
	// (unix seconds) to stay in the same range as the other Cat B loaders and
	// avoid the overflow branch that would otherwise never execute.
	return map[string]any{"installedRulePacks": packs}, timestampVersion(maxUpdated), nil
}
