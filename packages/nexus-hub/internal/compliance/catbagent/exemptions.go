package catbagent

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// AgentExemptionsLoader aggregates approved compliance_exemption_grant
// rows into the host-list shape the desktop Agent's exemption.Store
// consumes. Cat B loader for (thingType=agent, configKey=exemptions).
//
// Shape returned by Load matches packages/agent/internal/policy/exemption/store.go
// (ApplyShadowState):
//
//	{
//	    "admin_exemptions": [host, ...],
//	    "denylist":         [host, ...]
//	}
//
// The agent treats this as the authoritative admin allowlist — its
// SetAllowlist call wipes any prior admin entries and replaces them
// with this list (auto-exempted hosts are preserved). An empty
// admin_exemptions list therefore IS authoritative ("remove all
// admin grants"), distinct from the Cat B null/empty-object no-op
// convention.
//
// `denylist` is currently always empty — no CP-side data model row
// expresses "host the agent must NEVER auto-exempt"; the agent's
// denylist today is local yaml. When CP grows a denylist surface,
// project it here.
//
// Per-agent scoping is intentionally not implemented yet: every
// agent sees every active grant. The compliance_exemption_grant table
// has no Thing/DeviceGroup column today; when one is added, scope the
// WHERE clause here without changing the wire shape.
type AgentExemptionsLoader struct {
	db     pgxQuerier
	logger *slog.Logger
}

// NewAgentExemptionsLoader constructs a loader bound to the given pool.
// The logger is optional; when nil a discard logger is used.
func NewAgentExemptionsLoader(db pgxQuerier, logger *slog.Logger) *AgentExemptionsLoader {
	return &AgentExemptionsLoader{db: db, logger: logger}
}

// agentExemptionsState mirrors the JSON shape exemption.Store.ApplyShadowState
// parses. JSON tags MUST stay in sync with that struct.
type agentExemptionsState struct {
	AdminExemptions []string `json:"admin_exemptions"`
	Denylist        []string `json:"denylist"`
}

// agentExemptionsSelect returns the distinct target_host of every grant
// active at $1. NOT inactive AND effective_from <= now AND expires_at > now
// is the same eligibility predicate the compliance-proxy Cat B loader
// (LoadActiveExemptions in compliance-proxy/internal/config/loaders/
// exemptions.go) uses, so the agent and the proxy see the same set of
// approved hosts (proxy gets per-(IP, host) pairs, agent gets the
// distinct host list).
//
// Column order MUST stay in sync with the rows.Scan call below.
const agentExemptionsSelect = `
	SELECT DISTINCT target_host, GREATEST(MAX(updated_at), MAX(activated_at)) AS latest
	FROM compliance_exemption_grant
	WHERE NOT inactive
	  AND effective_from <= $1
	  AND expires_at > $1
	GROUP BY target_host
	ORDER BY target_host ASC
`

// Load returns the agent exemption state plus a version derived from
// the greatest updated_at/activated_at across the active set. thingID
// is accepted for interface uniformity but not yet used (no per-agent
// scoping today).
func (l *AgentExemptionsLoader) Load(ctx context.Context, _ string) (any, int64, error) {
	now := time.Now().UTC()
	rows, err := l.db.Query(ctx, agentExemptionsSelect, now)
	if err != nil {
		return nil, 0, fmt.Errorf("catb: query agent exemptions: %w", err)
	}
	defer rows.Close()

	admins := make([]string, 0)
	var maxUpdated time.Time
	for rows.Next() {
		var (
			host   string
			latest *time.Time
		)
		if err := rows.Scan(&host, &latest); err != nil {
			return nil, 0, fmt.Errorf("catb: scan agent exemptions: %w", err)
		}
		admins = append(admins, host)
		if latest != nil && latest.After(maxUpdated) {
			maxUpdated = *latest
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("catb: iterate agent exemptions: %w", err)
	}

	state := agentExemptionsState{
		AdminExemptions: admins,
		Denylist:        []string{},
	}
	return state, timestampVersion(maxUpdated), nil
}
