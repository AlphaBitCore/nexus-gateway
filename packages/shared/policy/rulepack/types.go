package rulepack

import "time"

// Pack mirrors the rule_pack + rule tables as a single in-memory document.
// Used both by the YAML loader (authored format) and by the DB reader.
//
// yaml:"-" on DB-only fields (ID, CreatedAt) keeps the authored YAML shape
// clean and avoids collisions with authored keys.
type Pack struct {
	ID          string    `json:"id,omitempty" yaml:"-"`
	Name        string    `json:"name" yaml:"name"`
	Version     string    `json:"version" yaml:"version"`
	Maintainer  string    `json:"maintainer" yaml:"maintainer"`
	Description string    `json:"description,omitempty" yaml:"description,omitempty"`
	Signature   string    `json:"signature,omitempty" yaml:"signature,omitempty"`
	CreatedAt   time.Time `json:"createdAt,omitempty" yaml:"-"`
	Rules       []Rule    `json:"rules" yaml:"rules"`
}

// Rule is a single pattern entry. RuleID is pack-local (e.g. "pi-001").
// DB-only fields (ID, PackID) are excluded from YAML via yaml:"-" so the
// authored "id" key maps unambiguously to RuleID.
type Rule struct {
	ID          string   `json:"id,omitempty" yaml:"-"`
	PackID      string   `json:"packId,omitempty" yaml:"-"`
	RuleID      string   `json:"ruleId" yaml:"id"`
	Category    string   `json:"category" yaml:"category"`
	Severity    string   `json:"severity" yaml:"severity"`
	Pattern     string   `json:"pattern" yaml:"pattern"`
	Flags       string   `json:"flags,omitempty" yaml:"flags,omitempty"`
	Description string   `json:"description,omitempty" yaml:"description,omitempty"`
	Labels      []string `json:"labels,omitempty" yaml:"labels,omitempty"`
}

// Install describes a RulePackInstall row + its resolved pack.
type Install struct {
	ID          string    `json:"id"`
	PackID      string    `json:"packId"`
	PackName    string    `json:"packName"`
	PinVersion  string    `json:"pinVersion"`
	BoundHookID string    `json:"boundHookId"`
	Enabled     bool      `json:"enabled"`
	InstalledAt time.Time `json:"installedAt"`
}

// Override is a per-install per-rule modifier.
type Override struct {
	ID               string `json:"id"`
	InstallID        string `json:"installId"`
	RuleLocalID      string `json:"ruleLocalId"`
	Disabled         bool   `json:"disabled"`
	SeverityOverride string `json:"severityOverride,omitempty"`
}

// Match is the product of Evaluator.Evaluate. It carries enough identity
// to fill traffic_event.blocking_rule {pack, pack_version, rule_id} and
// merge labels into the caller hook's Tags.
type Match struct {
	PackName    string   `json:"pack"`
	PackVersion string   `json:"packVersion"`
	RuleLocalID string   `json:"ruleId"`
	Category    string   `json:"category"`
	Severity    string   `json:"severity"`
	Labels      []string `json:"labels"`
	MatchedText string   `json:"matchedText,omitempty"`
}

// EffectiveRuleSet is the post-override view of a single Install.
type EffectiveRuleSet struct {
	Install Install `json:"install"`
	Pack    Pack    `json:"pack"`
}

// BlockingRule is the JSONB shape written to traffic_event.blocking_rule.
// JSON tags deliberately use snake_case to match the audit schema.
type BlockingRule struct {
	Pack        string `json:"pack"`
	PackVersion string `json:"pack_version"`
	RuleID      string `json:"rule_id"`
}
