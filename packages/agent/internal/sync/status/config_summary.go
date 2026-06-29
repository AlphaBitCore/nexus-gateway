package status

import (
	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// ConfigSummary is the slice of thingclient state the menu-bar UI renders in
// its Configuration panel. Field tags are camelCase to match the rest of the
// StatusSnapshot JSON contract consumed by the Swift / C++ clients.
type ConfigSummary struct {
	// ThingVersion is the desired config version Hub has for this Thing.
	ThingVersion int64 `json:"thingVersion"`
	// ReportedVersion is the last version the data plane successfully applied
	// and acknowledged back to Hub via shadow_report.
	ReportedVersion int64 `json:"reportedVersion"`
	// InSync is true when ThingVersion == ReportedVersion — i.e. every
	// desired-state mutation has been observed + applied locally.
	InSync bool `json:"inSync"`
	// LastReportedAt is the RFC3339 timestamp of the most recent successful
	// shadow_report. Empty string when no report has been sent.
	LastReportedAt string `json:"lastReportedAt"`
	// HooksEnabled is the count of hook entries in the current hooks
	// template whose enabled field is true.
	HooksEnabled int `json:"hooksEnabled"`
	// InterceptionDomains is the count of entries in the interception_domains
	// template whose enabled field is true (or all entries when the wire
	// shape carries no enabled flag — see schema notes below).
	InterceptionDomains int `json:"interceptionDomains"`
	// ActiveExemptions is the count of entries in the exemptions
	// template's active array (i.e. currently enforced exemption records).
	// Kept as a count alongside the full Exemptions list below for
	// backward compatibility with old menu-bar clients that read just
	// the number; new clients should prefer Exemptions for detail.
	ActiveExemptions int `json:"activeExemptions"`
	// Exemptions carries the brief-form active exemption entries (id,
	// host, reason, until). Sourced from exemptions (Cat B
	// pulled — see configdispatch.go) so the menu / Overview can show
	// the same details users see on the Policies page without a second
	// IPC round-trip.
	Exemptions []ExemptionBrief `json:"exemptions"`
	// RulePacks carries the installed rule packs as brief entries
	// (id, name, version). Sourced from installed_rule_packs (Cat B
	// pulled). Frontend derives count via len(RulePacks).
	RulePacks []RulePackBrief `json:"rulePacks"`
	// KillSwitch is the current killswitch state — whether engaged,
	// who toggled it (user-paused / admin / fleet) and any actor
	// metadata. Sourced from killswitch (Cat A inline).
	KillSwitch KillSwitchBrief `json:"killSwitch"`
}

// ExemptionBrief is the menu-summary shape for a single active
// exemption row. Mirrors policies.ExemptionView's user-facing fields
// without dragging in the policies package.
type ExemptionBrief struct {
	ID     string `json:"id,omitempty"`
	Host   string `json:"host,omitempty"`
	Reason string `json:"reason,omitempty"`
	Until  string `json:"until,omitempty"`
}

// RulePackBrief is the menu-summary shape for a single installed rule
// pack. Just the human-readable fields — caller can issue
// GET_APPLIED_CONFIG for the rule list when drilling into details.
type RulePackBrief struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

// KillSwitchBrief carries enough of the killswitch shadow shape for
// the menu / Overview UI to render correctly without needing to
// re-parse the raw shadow JSON.
type KillSwitchBrief struct {
	// Engaged is true when interception is currently OFF (paused /
	// killed). Frontend renders the Policies "KILL SWITCH" card from
	// this single boolean.
	Engaged bool `json:"engaged"`
	// Actor identifies WHO triggered the current state — typical
	// values: "user-paused", "admin", "fleet", "" (default).
	Actor string `json:"actor,omitempty"`
	// Reason is the optional human-readable explanation pushed by
	// the actor (admin reason text or auto-pause cause).
	Reason string `json:"reason,omitempty"`
}

// ThingStateAccessor is the subset of *thingclient.Client the status layer
// reads to build a ConfigSummary. Kept narrow so tests can substitute a fake
// without pulling in the full WebSocket-backed client.
type ThingStateAccessor interface {
	SnapshotDesired() map[string]thingclient.ConfigState
	DesiredVer() int64
	ReportedVer() int64
	LastReportedAt() string
}

// BuildConfigSummary extracts the subset of desired-state data the menu-bar
// Configuration panel renders. Missing keys and malformed payloads produce
// zero counts rather than errors so the UI contract stays stable.
//
// A nil accessor (e.g. agent started with HubURL unset) returns a zero-value
// summary — callers can embed the result directly in StatusSnapshot without a
// guard.
// BuildConfigSummary populates the menu-bar ConfigSummary view.
// cacheGet is the Cat B (HTTP-pulled) snapshot reader — pass nil for
// pre-cache callers (tests, early boot). When non-nil it takes priority
// over the thingclient desired snapshot for keys that are HTTP-pulled,
// matching the same pick() pattern policies.Build uses for the
// Policies page. Without this fix the menu-bar always reported 0
// for interception_domains / hooks / exemptions even
// when those configs were live (without this fix the counts were always 0).
func BuildConfigSummary(tc ThingStateAccessor, cacheGet func(key string) json.RawMessage) ConfigSummary {
	if tc == nil {
		return ConfigSummary{}
	}

	s := ConfigSummary{
		ThingVersion:    tc.DesiredVer(),
		ReportedVersion: tc.ReportedVer(),
		LastReportedAt:  tc.LastReportedAt(),
	}
	s.InSync = s.ThingVersion == s.ReportedVersion

	snap := tc.SnapshotDesired()
	// pick: cache (Cat B HTTP-pulled snapshot) → fall back to thingclient
	// desired snapshot (Cat A inline state). Mirrors the lookup order
	// policies.Build() uses so the Policies page and the ConfigSummary
	// pull from the same source of truth.
	pick := func(key string) json.RawMessage {
		if cacheGet != nil {
			if v := cacheGet(key); len(v) > 0 {
				return v
			}
		}
		return snap[key].State
	}
	s.HooksEnabled = countEnabledHooks(pick("hooks"))
	s.InterceptionDomains = countInterceptionDomains(pick("interception_domains"))
	s.Exemptions = parseExemptionsBrief(snap["exemptions"].State)
	s.ActiveExemptions = len(s.Exemptions)
	s.RulePacks = parseRulePacksBrief(pick("installed_rule_packs"))
	s.KillSwitch = parseKillSwitchBrief(snap["killswitch"].State)
	return s
}

// rulePacksPayload accepts the canonical Hub Cat B shape for
// installed_rule_packs:
//
//	{"rulePacks": [{id, name, version, ...}, ...]}
//
// Older / debug shapes carrying just ["pack-id-1", "pack-id-2"] are
// also tolerated so a pre-Hub-rollout agent never reports an empty
// list by accident.
type rulePackEntry struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

type rulePacksPayload struct {
	RulePacks []rulePackEntry `json:"rulePacks"`
	Packs     []rulePackEntry `json:"packs"`
}

func parseRulePacksBrief(raw json.RawMessage) []RulePackBrief {
	out := []RulePackBrief{}
	if len(raw) == 0 {
		return out
	}
	var p rulePacksPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return out
	}
	src := p.RulePacks
	if len(src) == 0 {
		src = p.Packs
	}
	for _, r := range src {
		out = append(out, RulePackBrief(r))
	}
	return out
}

// killswitchPayload mirrors the Cat A killswitch shadow shape. The
// shadow key is binary: engaged=true means the kill switch is engaged
// (paused / admin-stopped); engaged=false means normal traffic flow.
// The brief surfaces the same boolean to the UI verbatim.
type killswitchPayload struct {
	Engaged *bool  `json:"engaged,omitempty"`
	Actor   string `json:"actor,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

func parseKillSwitchBrief(raw json.RawMessage) KillSwitchBrief {
	if len(raw) == 0 {
		return KillSwitchBrief{}
	}
	var p killswitchPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return KillSwitchBrief{}
	}
	out := KillSwitchBrief{Actor: p.Actor, Reason: p.Reason}
	if p.Engaged != nil && *p.Engaged {
		out.Engaged = true
	}
	return out
}

// exemptionEntry is the user-facing subset of an active exemption row.
type exemptionEntry struct {
	ID     string `json:"id,omitempty"`
	Host   string `json:"host,omitempty"`
	Reason string `json:"reason,omitempty"`
	Until  string `json:"until,omitempty"`
}

// activeExemptionsBriefPayload mirrors the {"active":[…], "entries":[…]}
// shape (see countActiveExemptions). We accept either array.
type activeExemptionsBriefPayload struct {
	Active  []exemptionEntry `json:"active"`
	Entries []exemptionEntry `json:"entries"`
}

func parseExemptionsBrief(raw json.RawMessage) []ExemptionBrief {
	out := []ExemptionBrief{}
	if len(raw) == 0 {
		return out
	}
	var p activeExemptionsBriefPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return out
	}
	src := p.Active
	if len(src) == 0 {
		src = p.Entries
	}
	for _, e := range src {
		out = append(out, ExemptionBrief(e))
	}
	return out
}

// hookEntry matches the minimum shape of a single hook config entry. Extra
// fields are ignored by json.Unmarshal.
type hookEntry struct {
	Enabled bool `json:"enabled"`
}

// hookConfigPayload is the inline shadow state for hooks.
//
// The Hub Cat B publisher emits the aggregated list under "hookConfigs"
// (matching the agent's AgentPipeline.ApplyHooksShadowState parser). Older
// builds and tests use "hooks". Accept either to stay forward-compatible
// during rollouts where the menu-bar app may be one version ahead of Hub.
type hookConfigPayload struct {
	HookConfigs []hookEntry `json:"hookConfigs"`
	Hooks       []hookEntry `json:"hooks"`
}

func countEnabledHooks(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var p hookConfigPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return 0
	}
	entries := p.HookConfigs
	if len(entries) == 0 {
		entries = p.Hooks
	}
	n := 0
	for _, h := range entries {
		if h.Enabled {
			n++
		}
	}
	return n
}

// interceptionDomainEntry matches the minimum shape of a single domain
// entry from either supported payload shape. enabledOmitted records the
// case where the wire entry carried no "enabled" field at all (e.g. the
// legacy {"domains": ["a.com", ...]} string form) so the counter can fall
// back to counting all entries — silently dropping all of them would make
// a fresh Hub roll-out look like a complete config wipe.
type interceptionDomainEntry struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// interceptionDomainsPayload accepts every shape Hub has emitted for the
// interception_domains shadow key:
//
//   - {"interceptionDomains": [ {DTO with enabled:bool}, ... ]}
//     The current Hub Cat B publisher (see shadow.InterceptionDomainDTO
//     and AgentPipeline.ApplyDomainsShadowState).
//   - {"domains": [ "a.com", "b.com" ]}
//     A legacy string-list shape some early prototypes used.
//
// Either array works for the count; the menu-bar summary intentionally
// reports the same number the operator would see in the admin UI.
type interceptionDomainsPayload struct {
	InterceptionDomains []interceptionDomainEntry `json:"interceptionDomains"`
	Domains             []json.RawMessage         `json:"domains"`
}

func countInterceptionDomains(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var p interceptionDomainsPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return 0
	}
	if len(p.InterceptionDomains) > 0 {
		// New shape: report only entries that are explicitly enabled. An
		// entry missing the field is treated as enabled to keep parity
		// with admin-UI "show all" defaults.
		n := 0
		for _, d := range p.InterceptionDomains {
			if d.Enabled == nil || *d.Enabled {
				n++
			}
		}
		return n
	}
	return len(p.Domains)
}
