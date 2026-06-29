package core

import (
	"github.com/goccy/go-json"
	"time"
)

// Configuration/control wire structs: the model catalog + providers, routing
// rules, and the kill-switch / emergency-passthrough control surfaces.

// ModelCatalog is the {data:[...]} envelope from GET /api/admin/models,
// grouped by provider.
type ModelCatalog struct {
	Data []ModelGroup `json:"data"`
}

// ModelGroup is one provider and its models. The provider sub-object carries
// only id/name/displayName here (no enabled flag), but it decodes into the same
// Provider type the providers list uses — one provider shape, not two.
type ModelGroup struct {
	Provider Provider `json:"provider"`
	Models   []Model  `json:"models"`
}

// ProvidersResult is GET /api/admin/providers ({data:[...],total}).
type ProvidersResult struct {
	Data []Provider `json:"data"`
}

// Provider is an upstream provider. Name is the unique catalog key (it matches
// traffic_event.provider_name and the latency-phases provider groupKey);
// DisplayName is the human-friendly label shown to operators; the drill resolves
// Name → ID to call ProviderDetail but never shows the UUID. Enabled is set only
// on the providers list (the model catalog's nested provider ref leaves it false).
type Provider struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Enabled     bool   `json:"enabled"`
}

// Label returns the human-friendly provider label, falling back to the code.
func (p Provider) Label() string {
	if p.DisplayName != "" {
		return p.DisplayName
	}
	return p.Name
}

// Model is one catalog model row (subset of the admin fields the toolkit uses).
type Model struct {
	ID                    string  `json:"id"`
	Code                  string  `json:"code"`
	Name                  string  `json:"name"`
	ProviderID            string  `json:"providerId"`
	Type                  string  `json:"type"`
	Status                string  `json:"status"`
	Enabled               bool    `json:"enabled"`
	MaxContextTokens      int     `json:"maxContextTokens"`
	InputPricePerMillion  float64 `json:"inputPricePerMillion"`
	OutputPricePerMillion float64 `json:"outputPricePerMillion"`
}

// KillSwitchResult is the response from POST /api/admin/compliance/killswitch.
type KillSwitchResult struct {
	Engaged        bool `json:"engaged"`
	Version        int  `json:"version"`
	ThingsNotified int  `json:"thingsNotified"`
	ThingsOnline   int  `json:"thingsOnline"`
}

// KillSwitchState is the current global kill-switch state, derived from the newest
// killswitch config-change event. Known is false when the switch has never been
// toggled (no event), so the UI distinguishes "off" from "never set" rather than
// showing a misleading default.
type KillSwitchState struct {
	Engaged bool
	Known   bool
	Version int
	At      string // event timestamp (RFC3339)
	By      string // actor who last toggled it
}

// PassthroughTier is one tier of the emergency-passthrough config — the global
// tier, or a single per-adapter / per-provider override.
type PassthroughTier struct {
	Enabled         bool   `json:"enabled"`
	BypassHooks     bool   `json:"bypassHooks"`
	BypassCache     bool   `json:"bypassCache"`
	BypassNormalize bool   `json:"bypassNormalize"`
	ExpiresAt       string `json:"expiresAt,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

// active reports whether this tier is bypassing anything right now.
func (t PassthroughTier) active() bool {
	return t.Enabled && (t.BypassHooks || t.BypassCache || t.BypassNormalize)
}

// PassthroughSnapshot is the full three-tier emergency-passthrough state: the
// global tier plus any per-adapter and per-provider overrides. ProviderNames maps
// a provider id to its display name so overrides surface by name, not bare id.
type PassthroughSnapshot struct {
	Global        PassthroughTier            `json:"global"`
	Adapters      map[string]PassthroughTier `json:"adapters"`
	Providers     map[string]PassthroughTier `json:"providers"`
	ProviderNames map[string]string          `json:"providerNames"`
}

// ActiveOverrides counts the per-adapter and per-provider tiers currently
// bypassing something — the "is anything slipping past compliance" signal.
func (s PassthroughSnapshot) ActiveOverrides() (adapters, providers int) {
	for _, t := range s.Adapters {
		if t.active() {
			adapters++
		}
	}
	for _, t := range s.Providers {
		if t.active() {
			providers++
		}
	}
	return adapters, providers
}

// PassthroughGlobalRequest is the body for PUT /api/admin/passthrough/global.
// When Enabled, the server requires a future ExpiresAt (≤ 8h out) and a reason of
// at least 20 characters, and rejects BypassNormalize without BypassCache;
// SetPassthroughGlobal fills server-valid defaults for any of these the caller
// omits, so an engage never fails on a missing invariant.
type PassthroughGlobalRequest struct {
	Enabled         bool       `json:"enabled"`
	BypassHooks     bool       `json:"bypassHooks"`
	BypassCache     bool       `json:"bypassCache"`
	BypassNormalize bool       `json:"bypassNormalize"`
	ExpiresAt       *time.Time `json:"expiresAt,omitempty"`
	Reason          string     `json:"reason,omitempty"`
}

// RoutingSimulateRequest is the body for POST /api/admin/routing-rules/simulate.
type RoutingSimulateRequest struct {
	ModelID      string `json:"modelId"`
	EndpointType string `json:"endpointType"`
}

// RoutingSimulateResult is the routing dry-run outcome ("why this route"). It
// fires no real request.
type RoutingSimulateResult struct {
	Substituted     bool            `json:"substituted"`
	RuleName        string          `json:"ruleName"`
	Targets         []RoutingTarget `json:"targets"`
	RecoveryTargets []RoutingTarget `json:"recoveryTargets"`
	Warnings        []string        `json:"warnings"`
}

// RoutingTarget is one resolved provider/model in the route.
type RoutingTarget struct {
	ProviderName    string `json:"providerName"`
	ModelCode       string `json:"modelCode"`
	ModelName       string `json:"modelName"`
	ProviderModelID string `json:"providerModelId"`
}

// routingRuleList is the {data:[...]} envelope wrapping the routing-rule list.
type routingRuleList struct {
	Data []RoutingRule `json:"data"`
}

// RoutingRule is one row of GET /api/admin/routing-rules. The toolkit shows the
// identity + ordering + enabled state; the strategy config blobs are not needed
// for the toggle surface, so they are deliberately not decoded here.
type RoutingRule struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	StrategyType  string `json:"strategyType"`
	Priority      int    `json:"priority"`
	PipelineStage int    `json:"pipelineStage"`
	Enabled       bool   `json:"enabled"`
	// Detail-only fields the list omits but the detail drawer surfaces so an
	// operator can read what a rule does during an incident without leaving the
	// TUI. The three predicate/config blobs are arbitrary JSON kept raw and
	// pretty-printed on render.
	Description     string          `json:"description"`
	Config          json.RawMessage `json:"config"`
	MatchConditions json.RawMessage `json:"matchConditions"`
	FallbackChain   json.RawMessage `json:"fallbackChain"`
	CreatedAt       string          `json:"createdAt"`
	UpdatedAt       string          `json:"updatedAt"`
}
