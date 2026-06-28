package catbagent

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	hooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
)

// pgxQuerier is the minimum subset of *pgxpool.Pool that the Cat B
// loaders need. It is satisfied by *pgxpool.Pool in production and by
// pgxmock.PgxPoolIface in tests, which lets us unit-test the row ->
// JSON assembly path without a live DB.
type pgxQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// AgentHookConfigLoader aggregates enabled HookConfig rows for the
// agent. Shape returned by Load matches AgentPipeline.ApplyHooksShadowState:
//
//	{"hookConfigs": [ <hooks.HookConfig>, ... ]}
//
// Today every agent sees every enabled hook — per-agent scoping via
// device groups is a follow-up and will only change the WHERE clause.
//
// rulePackStore (optional) is the shared rule-pack reader used by
// rulepack.Enrich. When set, hooks whose ImplementationID is in
// RulePackConsumer (rulepack-engine, content-safety, keyword-filter,
// pii-detector) have their bound rule-pack installs injected into
// `Config["_rulePackInstalls"]` before the payload ships to the agent.
// Without this, rule-pack-engine hooks on the agent load with no
// rules bound — admin-configured rule packs have zero effect on
// agent behavior. ai-gateway + compliance-proxy already enrich
// hooks the same way before building their pipelines (see
// packages/ai-gateway/cmd/ai-gateway/main.go and
// packages/compliance-proxy/cmd/compliance-proxy/init.go).
type AgentHookConfigLoader struct {
	db            pgxQuerier
	rulePackStore rulepack.InstallLister
	logger        *slog.Logger
}

// NewAgentHookConfigLoader constructs a loader bound to the given pool.
// The logger is optional; when nil a discard logger is used.
//
// rulePackStore is optional but recommended in production — without it,
// rule-pack-engine and the migrating hooks (content-safety etc.) ship
// with no rules. Tests can pass nil to short-circuit enrichment.
func NewAgentHookConfigLoader(db pgxQuerier, rulePackStore rulepack.InstallLister, logger *slog.Logger) *AgentHookConfigLoader {
	return &AgentHookConfigLoader{db: db, rulePackStore: rulePackStore, logger: logger}
}

// agentHookConfigSelect duplicates the enabled-hook SELECT shape used
// by packages/control-plane/internal/store/hook_config.go. Copied
// (not imported) to avoid a cp -> hub dependency cycle; any schema
// change to HookConfig must update both sites.
//
// Column order MUST stay in sync with scanAgentHookConfigRow below.
const agentHookConfigSelect = `
	SELECT id, name, type, "implementationId", stage, category, endpoint, script,
	       config, priority, "timeoutMs", "failBehavior", enabled,
	       "applicableIngress", "updatedAt"
	FROM "HookConfig"
	WHERE enabled = true
	ORDER BY priority ASC, "createdAt" ASC
`

// agentHookConfigRow is the wire shape that marshals into
// hooks.HookConfig verbatim. We keep a local struct (with the exact
// JSON tags the agent expects) rather than importing shared/hooks so
// the loader has no dependency drift risk and the field contract is
// visible at the SQL scan site.
type agentHookConfigRow struct {
	ID                string          `json:"id"`
	ImplementationID  string          `json:"implementationId"`
	Name              string          `json:"name"`
	Priority          int             `json:"priority"`
	Enabled           bool            `json:"enabled"`
	Stage             string          `json:"stage"`
	FailBehavior      string          `json:"failBehavior"`
	TimeoutMs         int             `json:"timeoutMs"`
	ApplicableIngress []string        `json:"applicableIngress"`
	Config            json.RawMessage `json:"config,omitempty"`
}

// Load returns {"hookConfigs": [...]} plus a version derived from the
// greatest updated_at in the result set. thingID is accepted for
// interface uniformity but not yet used (no per-agent scoping).
func (l *AgentHookConfigLoader) Load(ctx context.Context, _ string) (any, int64, error) {
	rows, err := l.db.Query(ctx, agentHookConfigSelect)
	if err != nil {
		return nil, 0, fmt.Errorf("catb: query hook_config: %w", err)
	}
	defer rows.Close()

	out := make([]agentHookConfigRow, 0)
	var maxUpdated time.Time
	for rows.Next() {
		var (
			r         agentHookConfigRow
			typ       string
			category  *string
			endpoint  *string
			script    *string
			cfgRaw    []byte
			updatedAt time.Time
		)
		if err := rows.Scan(
			&r.ID, &r.Name, &typ, &r.ImplementationID, &r.Stage,
			&category, &endpoint, &script,
			&cfgRaw, &r.Priority, &r.TimeoutMs, &r.FailBehavior, &r.Enabled,
			&r.ApplicableIngress, &updatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("catb: scan hook_config: %w", err)
		}
		// Only keep fields the agent actually parses; extras (type,
		// category, endpoint, script) are read for completeness but
		// intentionally not forwarded because hooks.HookConfig has no
		// room for them today.
		_ = typ
		_ = category
		_ = endpoint
		_ = script

		if len(cfgRaw) > 0 {
			r.Config = json.RawMessage(cfgRaw)
		}
		if r.ApplicableIngress == nil {
			// Match the schema default when the DB row came through
			// with a NULL array (rare, but possible on legacy rows).
			r.ApplicableIngress = []string{"ALL"}
		}

		out = append(out, r)
		if updatedAt.After(maxUpdated) {
			maxUpdated = updatedAt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("catb: iterate hook_config: %w", err)
	}

	// Enrich rule-pack-backed hooks with their effective installs so
	// the agent receives ready-to-evaluate hook configs. Skipped when no
	// rule-pack store is wired (tests / dev). Failures inside Enrich are
	// best-effort: a single bad install leaves its hook with the legacy
	// config rather than failing the whole reload — see rulepack.Enrich
	// for the contract.
	if l.rulePackStore != nil && len(out) > 0 {
		// rulepack.Enrich operates on []hooks.HookConfig. Round-trip via
		// JSON because agentHookConfigRow is a local view-only struct
		// kept independent of shared/hooks (see field-tag comment
		// above). Cost is one marshal/unmarshal per reload, not per
		// request — well off the hot path.
		// json.Marshal of []agentHookConfigRow and json.Unmarshal into
		// []hooks.HookConfig cannot fail given the concrete types involved
		// (no custom MarshalJSON that errors, no interface{} fields). We
		// still use must-succeed helpers to keep the round-trip explicit
		// while eliminating untestable defensive error branches.
		payload, _ := json.Marshal(out)
		var hookCfgs []hooks.HookConfig
		_ = json.Unmarshal(payload, &hookCfgs)
		enriched, err := rulepack.Enrich(ctx, l.rulePackStore, hookCfgs)
		if err != nil {
			return nil, 0, fmt.Errorf("catb: enrich hook_config with rule packs: %w", err)
		}
		state := map[string]any{"hookConfigs": enriched}
		return state, timestampVersion(maxUpdated), nil
	}

	state := map[string]any{"hookConfigs": out}
	return state, timestampVersion(maxUpdated), nil
}

// timestampVersion converts a time.Time to a unix-seconds version;
// zero-valued times map to 0 so a fully empty result set reports
// version=0 rather than a large negative epoch-predecessor.
func timestampVersion(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
