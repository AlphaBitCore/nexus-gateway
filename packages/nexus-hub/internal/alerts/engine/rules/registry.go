// Package rules exposes the code-owned default definitions for all built-in
// alert rules. These defaults are the source of truth for the
// POST /api/v1/admin/alerts/rules/{id}/reset endpoint: reset writes these
// values back to the AlertRule DB row, discarding any operator edits.
//
// The TS seed at tools/db-migrate/seed/seed-alerting.ts plants the same
// definitions at `prisma db seed` time. Both files must stay in lockstep;
// TestBuiltinRulesMatchSeed is the lockstep gate.
package rules

import (
	"fmt"
	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

// RuleDef is the code-owned default definition of a built-in alert rule.
type RuleDef struct {
	ID              string // "quota.threshold", etc.
	DisplayName     string
	SourceType      string            // "quota", "proxy", "thing", "provider", "auth", "system"
	DefaultSeverity alerting.Severity // lowercase: "critical", "high", "medium", "low", "info"
	RequiresAck     bool
	Enabled         bool
	CooldownSec     int
	Params          json.RawMessage // stored as JSON to avoid drift vs TS seed
	ParamsSchema    json.RawMessage // JSON Schema for Params
}

// Registry is a lookup table keyed by rule id.
type Registry struct {
	defs []RuleDef
	byID map[string]RuleDef
}

// NewRegistry constructs a Registry from the given rules. Typical use is
// `rules.NewRegistry(rules.BuiltinRules)`.
func NewRegistry(defs []RuleDef) *Registry {
	byID := make(map[string]RuleDef, len(defs))
	cp := make([]RuleDef, len(defs))
	copy(cp, defs)
	for _, d := range cp {
		byID[d.ID] = d
	}
	return &Registry{defs: cp, byID: byID}
}

// Lookup returns the RuleDef for the given id. Returns false if no such rule.
func (r *Registry) Lookup(id string) (RuleDef, bool) {
	d, ok := r.byID[id]
	return d, ok
}

// All returns every rule def, in the order BuiltinRules was passed in.
func (r *Registry) All() []RuleDef {
	out := make([]RuleDef, len(r.defs))
	copy(out, r.defs)
	return out
}

// mustJSON panics if m cannot be marshaled. Used at package init to construct
// the BuiltinRules' Params/ParamsSchema — if a literal here is malformed,
// the process should not start.
func mustJSON(m any) json.RawMessage {
	b, err := json.Marshal(m)
	if err != nil {
		panic(fmt.Sprintf("rules: mustJSON: %v", err))
	}
	return b
}
