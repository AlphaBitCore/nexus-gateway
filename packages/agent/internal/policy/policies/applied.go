// Package policies builds the AppliedConfig snapshot the Dashboard's
// Policies page renders — a unified view of every admin-pushed
// configuration this device is currently honouring (interception
// domains, hook chain, exemptions, kill switch, device defaults,
// etc.). The data flows Hub → thingclient.OnConfigChanged →
// shadow snapshot; this package only DECODES that snapshot into
// shapes the GUI can render without each section needing its own
// shadow-key knowledge.
//
// Lenient parsing is the rule: an unknown wire shape, a missing key,
// or a malformed payload yields an empty section rather than an error
// — the Policies page renders empty states for each, and the user
// sees "Admin hasn't pushed any X yet" instead of a hard failure.
package policies

import (
	"encoding/json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// AppliedConfig is the full payload returned by the GET_APPLIED_CONFIG
// IPC. Each field is a separate Policies-page section; the frontend
// renders one card per non-empty field plus a placeholder for fields
// where this device has nothing applied.
type AppliedConfig struct {
	Sync SyncStatus `json:"sync"`

	InterceptionDomains []InterceptionDomainView `json:"interceptionDomains"`
	Hooks               []HookView               `json:"hooks"`
	Exemptions          []ExemptionView          `json:"exemptions"`
	DeviceDefaults      DeviceDefaultsView       `json:"deviceDefaults"`
	KillSwitch          KillSwitchView           `json:"killSwitch"`
	RulePacks           []RulePackView           `json:"rulePacks"`

	// UserContext + OrganizationTree are unconditional base data.
	// UserContext is who currently owns the device (DeviceAssignment →
	// NexusUser); OrganizationTree is the breadcrumb-style chain from
	// root → user's org.
	UserContext      *UserContextView   `json:"userContext,omitempty"`
	OrganizationTree []OrganizationView `json:"organizationTree,omitempty"`

	// Stretch sections — placeholders until later wiring.
	DeviceGroup *DeviceGroupView `json:"deviceGroup,omitempty"`
	DiagMode    *DiagModeView    `json:"diagMode,omitempty"`
}

// UserContextView mirrors the user_context.user JSON payload Hub serves.
type UserContextView struct {
	ID             string `json:"id"`
	DisplayName    string `json:"displayName"`
	Email          string `json:"email,omitempty"`
	Status         string `json:"status,omitempty"`
	Source         string `json:"source,omitempty"`
	OrganizationID string `json:"organizationId"`
}

// OrganizationView mirrors one node in the org breadcrumb chain.
type OrganizationView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Code        string `json:"code"`
	ParentID    string `json:"parentId,omitempty"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
	Timezone    string `json:"timezone,omitempty"`
}

// SyncStatus mirrors the menu-bar ConfigSummary's version triple so
// the Policies page can render the same "in-sync vs drifted" banner
// the menu already shows.
type SyncStatus struct {
	DesiredVersion  int64  `json:"desiredVersion"`
	ReportedVersion int64  `json:"reportedVersion"`
	InSync          bool   `json:"inSync"`
	LastReportedAt  string `json:"lastReportedAt"`
}

// InterceptionDomainView mirrors the full Hub-served interception_domain
// row so the Dashboard can render the same table columns + detail-page
// fields that CP-UI's compliance/interception page shows. The wire
// schema is set by packages/nexus-hub/internal/storage/store/catb_agent_interception_domains.go;
// any change to that loader's row struct must update this view + the
// parser. Nested Paths[] carries the per-path overrides admin set so
// the detail page can render them as a sub-table.
type InterceptionDomainView struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	HostPattern   string `json:"hostPattern"`
	HostMatchType string `json:"hostMatchType,omitempty"`
	AdapterID     string `json:"adapterId,omitempty"`
	Enabled       bool   `json:"enabled"`
	// Priority is always rendered (no omitempty) — 0 is a meaningful
	// admin-set value, not "missing". omitempty silently dropped it
	// from the wire and the Dashboard showed "—" instead of "0".
	Priority          int                    `json:"priority"`
	DefaultPathAction string                 `json:"defaultPathAction,omitempty"`
	OnAdapterError    string                 `json:"onAdapterError,omitempty"`
	NetworkZone       string                 `json:"networkZone,omitempty"`
	Paths             []InterceptionPathView `json:"paths,omitempty"`
}

// InterceptionPathView is a nested per-path rule under an interception
// domain — the user-side mirror of the path_pattern table CP-UI also
// renders inside the domain detail panel.
type InterceptionPathView struct {
	ID          string   `json:"id"`
	PathPattern []string `json:"pathPattern"`
	MatchType   string   `json:"matchType,omitempty"`
	Action      string   `json:"action,omitempty"`
	// Priority always rendered — see InterceptionDomainView.Priority.
	Priority int  `json:"priority"`
	Enabled  bool `json:"enabled"`
}

// HookView mirrors the full hooks row Hub serves. Stage replaces
// the older OnMatch field name — wire schema is set by
// packages/nexus-hub/internal/storage/store/catb_agent_hooks.go. Config
// is left as raw JSON so the detail page can pretty-print whatever
// per-hook settings admin authored (PII patterns, content-safety
// thresholds, prompt-cache TTL, etc.) without the agent needing a
// per-hook-type decoder.
type HookView struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	ImplementationID string `json:"implementationId,omitempty"`
	Stage            string `json:"stage,omitempty"`
	// Numeric fields are always emitted (no omitempty). 0 = "no priority
	// boost / no timeout" is a meaningful admin choice — the UI needs
	// to see it as 0, not silently drop it.
	Priority          int             `json:"priority"`
	FailBehavior      string          `json:"failBehavior,omitempty"`
	TimeoutMs         int             `json:"timeoutMs"`
	ApplicableIngress []string        `json:"applicableIngress,omitempty"`
	Config            json.RawMessage `json:"config,omitempty"`
	Enabled           bool            `json:"enabled"`
}

// ExemptionView is one row in the "Exemptions" section — a per-host
// or per-user grant that bypasses a hook. Fields are whatever the
// shadow carries; Reason surfaces the admin-provided justification.
type ExemptionView struct {
	ID     string `json:"id"`
	Host   string `json:"host,omitempty"`
	User   string `json:"user,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// DeviceDefaultsView reflects the agent_settings shadow key — fleet-
// wide knobs admin sets via CP UI's devices/device-defaults page.
// All fields are pointers (or default-tolerating types) so a partial
// agent_settings payload doesn't fabricate values that admin never
// set.
type DeviceDefaultsView struct {
	QuitAllowed *bool `json:"quitAllowed,omitempty"`
	// Reporting cadence in seconds. Zero means "use yaml default";
	// non-zero is the admin's explicit override.
	HeartbeatIntervalSec  int `json:"heartbeatIntervalSec,omitempty"`
	AuditDrainIntervalSec int `json:"auditDrainIntervalSec,omitempty"`
	ConfigSyncIntervalSec int `json:"configSyncIntervalSec,omitempty"`
	AuditBatchSize        int `json:"auditBatchSize,omitempty"`
	// Shutdown-warning settings (read-only here; admin owns the
	// values in CP UI).
	ShutdownWarningEnabled bool              `json:"shutdownWarningEnabled,omitempty"`
	ShutdownWarning        map[string]string `json:"shutdownWarning,omitempty"`
	// Updater channel admin pinned for this fleet — "stable" /
	// "beta". autoUpdateEnabled flips OS-wide updater on/off.
	AutoUpdateEnabled bool   `json:"autoUpdateEnabled,omitempty"`
	AutoUpdateChannel string `json:"autoUpdateChannel,omitempty"`
	LogLevel          string `json:"logLevel,omitempty"`
	// TrafficUploadLevel controls which flows reach Hub. Surfaced here
	// read-only so the agent UI Policies page can show the active
	// level alongside the other admin-pushed knobs.
	TrafficUploadLevel string `json:"trafficUploadLevel,omitempty"`
	// ThemeID names the theme pack the agent Dashboard should render
	// with — admin sets this fleet-wide via CP UI. Empty means "fall
	// through to the agent's local default". The agent Dashboard's
	// ThemeProvider treats a non-empty value here as authoritative
	// over localStorage, matching the trafficUploadLevel convention.
	ThemeID string `json:"themeId,omitempty"`
	// ForceQUICFallbackBundles is the macOS-only bundle-ID allowlist
	// the NE proxy reads to decide which UDP flows to close (forcing
	// QUIC→TCP downgrade). Surfaced here read-only so the Policies
	// page can show the operator what's actually being intercepted.
	ForceQUICFallbackBundles []string `json:"forceQUICFallbackBundles,omitempty"`
	// AttestationEnabled is the fleet toggle for agent traffic
	// attestation. When true, the agent signs outbound CONNECTs with its
	// Ed25519 attestation key so the compliance-proxy can transparently
	// tunnel verified flows (skipping its own MITM + hook pipeline).
	// Default false; the toggle is a perf optimization, not a security
	// gate — invalid / missing signatures fall back to the normal
	// full-MITM path at CP per architecture § 4.
	AttestationEnabled bool `json:"attestationEnabled,omitempty"`
}

// KillSwitchView reflects the fleet kill-switch state for this
// device. Engaged is the canonical wire-truth coming from the shadow:
// Engaged=true means the admin has engaged the kill switch via CP UI's
// infrastructure/kill-switch page and the daemon is in passthrough
// posture (bump disabled); Engaged=false (fail-safe default) means
// normal operation — bump is active. The field matches the
// interception.Killswitch{Engaged bool} wire schema.
type KillSwitchView struct {
	Engaged bool   `json:"engaged"`
	Reason  string `json:"reason,omitempty"`
}

// RulePackView mirrors the rule_pack_install JOIN rule_pack projection
// the Hub installed_rule_packs CatBLoader serves. Packs are a CP-side
// organisational concept that bundles hooks — the constituent rules
// flow to the agent via hooks so the agent runs them in-line, not
// as a separate execution unit. This view exists purely for the Policies
// page so the user can see "admin has pack X installed in hook Y on
// this device" without having to inspect every hook's config payload.
type RulePackView struct {
	ID          string `json:"id"`
	PackID      string `json:"packId,omitempty"`
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Maintainer  string `json:"maintainer,omitempty"`
	Description string `json:"description,omitempty"`
	BoundHookID string `json:"boundHookId,omitempty"`
	Enabled     bool   `json:"enabled"`
	// RuleCount always emitted — 0 is "no rules in this pack", a
	// legitimate (if unusual) state. omitempty would hide it from the UI.
	RuleCount   int    `json:"ruleCount"`
	InstalledAt string `json:"installedAt,omitempty"`
	// Rules carries the per-pack rule list (id, category, severity,
	// pattern, description, labels). The agent needs the rule definitions
	// for display even though runtime evaluation happens inside hooks.
	Rules []RulePackRule `json:"rules,omitempty"`
}

// RulePackRule mirrors one row of the rule table joined into the pack
// payload. The agent doesn't evaluate these directly (the in-line rule
// engine consumes them via hooks); this is the user-visible
// reference for the Rule Pack detail page.
type RulePackRule struct {
	ID          string   `json:"id"`
	RuleID      string   `json:"ruleId,omitempty"`
	Category    string   `json:"category,omitempty"`
	Severity    string   `json:"severity,omitempty"`
	Pattern     string   `json:"pattern,omitempty"`
	Flags       string   `json:"flags,omitempty"`
	Description string   `json:"description,omitempty"`
	Labels      []string `json:"labels,omitempty"`
}

// DeviceGroupView is a placeholder. The agent doesn't have a direct
// thingclient channel for its group_id today; the Hub stamps the
// group on the thing record at enrollment time but never pushes it
// back. To populate this section we'd either need a new Hub endpoint
// or a thingclient extension; tracked in #70.
type DeviceGroupView struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// DiagModeView reflects whether admin has enabled verbose diagnostic
// mode for this device (a temporary window during which the daemon
// raises log level and captures extra detail). Empty / nil means
// "off"; presence with Until = future-time means "on until X".
type DiagModeView struct {
	Active bool   `json:"active"`
	Until  string `json:"until,omitempty"`
}

// Builder is the entry point: given a thingclient snapshot accessor and
// the post-apply SnapshotCache, produce a fully-populated AppliedConfig.
// nil accessor returns a zero-value AppliedConfig (pre-enrollment / no
// thingclient). A nil cache is tolerated (callers without TeeApplier
// wiring) but Cat B keys will then read from thingclient's desired
// cache only, which is empty for HTTP-pulled keys — the Policies page
// will show zeros for those sections until a Hub WS push lands.
//
// Key selection (Cat A vs Cat B):
//   - Cat A keys (state inline in desired): agent_settings, killswitch,
//     diag_mode. Read directly from thingclient SnapshotDesired().
//   - Cat B keys (HTTP-pulled): interception_domains, hooks,
//     payload_capture, policy_rules, exemptions. Read from cache first;
//     fall back to thingclient SnapshotDesired() for back-compat with
//     installations that haven't been upgraded to the tee-wired path.
func Build(tc ThingStateAccessor, cache *SnapshotCache) AppliedConfig {
	out := AppliedConfig{
		// Always-present empty slices so the JSON shape is stable
		// (frontend can `events.length` without a null check).
		InterceptionDomains: []InterceptionDomainView{},
		Hooks:               []HookView{},
		Exemptions:          []ExemptionView{},
		RulePacks:           []RulePackView{},
	}
	if tc == nil {
		return out
	}
	out.Sync = SyncStatus{
		DesiredVersion:  tc.DesiredVer(),
		ReportedVersion: tc.ReportedVer(),
		InSync:          tc.DesiredVer() == tc.ReportedVer(),
		LastReportedAt:  tc.LastReportedAt(),
	}
	snap := tc.SnapshotDesired()

	pick := func(key string) json.RawMessage {
		if cache != nil {
			if v := cache.Get(key); len(v) > 0 {
				return v
			}
		}
		return snap[key].State
	}

	out.InterceptionDomains = parseInterceptionDomains(pick("interception_domains"))
	out.Hooks = parseHooks(pick("hooks"))
	out.Exemptions = parseExemptions(pick("exemptions"))
	out.KillSwitch = parseKillSwitch(snap["killswitch"].State)
	out.DeviceDefaults = parseDeviceDefaults(snap["agent_settings"].State)
	out.DiagMode = parseDiagMode(snap["agent_settings"].State)
	out.RulePacks = parseRulePacks(pick("installed_rule_packs"))
	out.UserContext, out.OrganizationTree = parseUserContext(pick("user_context"))

	return out
}

// ThingStateAccessor is the minimal thingclient surface this package
// needs. Kept narrow so tests can substitute a fake.
type ThingStateAccessor interface {
	SnapshotDesired() map[string]thingclient.ConfigState
	DesiredVer() int64
	ReportedVer() int64
	LastReportedAt() string
}

func parseInterceptionDomains(raw json.RawMessage) []InterceptionDomainView {
	out := []InterceptionDomainView{}
	if len(raw) == 0 {
		return out
	}
	// Hub wire shape (canonical):
	//   {"interceptionDomains": [{id, name, hostPattern, hostMatchType,
	//                             adapterId, enabled, priority,
	//                             defaultPathAction, onAdapterError,
	//                             networkZone, paths: [{...}]}]}
	// Legacy fallback for early prototypes: {"domains": ["x", "y"]}.
	var wrapped struct {
		InterceptionDomains []InterceptionDomainView `json:"interceptionDomains"`
		Domains             []string                 `json:"domains"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return out
	}
	out = append(out, wrapped.InterceptionDomains...)
	for _, s := range wrapped.Domains {
		out = append(out, InterceptionDomainView{HostPattern: s, Enabled: true})
	}
	return out
}

func parseHooks(raw json.RawMessage) []HookView {
	out := []HookView{}
	if len(raw) == 0 {
		return out
	}
	// Hub wire shape (canonical, from catb_agent_hooks.go):
	//   {"hookConfigs": [{id, implementationId, name, priority, enabled,
	//                     stage, failBehavior, timeoutMs,
	//                     applicableIngress, config}]}
	// The legacy "hooks" key is kept for early prototypes that may
	// still emit it. Disabled hooks are returned too (Enabled=false)
	// so the UI can show them with a muted badge — the previous parser
	// filtered them out which made admin's intent invisible.
	var wrapped struct {
		HookConfigs []HookView `json:"hookConfigs"`
		Hooks       []HookView `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return out
	}
	entries := wrapped.HookConfigs
	if len(entries) == 0 {
		entries = wrapped.Hooks
	}
	out = append(out, entries...)
	return out
}

func parseExemptions(raw json.RawMessage) []ExemptionView {
	out := []ExemptionView{}
	// Wire shapes accepted from the "exemptions" shadow key:
	//
	//   { "admin_exemptions": ["host1", "host2", ...],
	//     "denylist":         ["host3", ...] }
	//       — current shape that exemption.Store.ApplyShadowState
	//         actually consumes. Strings are bare hosts; the only
	//         metadata is which list they came from.
	//   { "active":  [ {id, host, user, reason}, ... ] }
	//   { "entries": [ {id, host, user, reason}, ... ] }
	//       — richer shapes the menu-bar ConfigSummary tolerated;
	//         keeping the support so a future schema bump that
	//         re-introduces richer fields doesn't immediately
	//         blank out the Policies page.
	if len(raw) == 0 {
		return out
	}
	var wrapped struct {
		// Bare-host shape (canonical for the agent's
		// exemption.Store today).
		AdminExemptions []string `json:"admin_exemptions"`
		Denylist        []string `json:"denylist"`
		// Richer shapes (legacy / future).
		Active []struct {
			ID     string `json:"id"`
			Host   string `json:"host"`
			User   string `json:"user"`
			Reason string `json:"reason"`
		} `json:"active"`
		Entries []struct {
			ID     string `json:"id"`
			Host   string `json:"host"`
			User   string `json:"user"`
			Reason string `json:"reason"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return out
	}
	// Bare-host shape: build one ExemptionView per host. Reason is
	// "admin grant" so the Policies card distinguishes them from
	// auto-exempt (cert-pin) entries the agent generates locally.
	for _, host := range wrapped.AdminExemptions {
		out = append(out, ExemptionView{
			ID:     "admin:" + host,
			Host:   host,
			Reason: "admin grant",
		})
	}
	// Richer shapes fan out as before.
	entries := wrapped.Active
	if len(entries) == 0 {
		entries = wrapped.Entries
	}
	for _, e := range entries {
		out = append(out, ExemptionView{ID: e.ID, Host: e.Host, User: e.User, Reason: e.Reason})
	}
	return out
}

func parseKillSwitch(raw json.RawMessage) KillSwitchView {
	// Empty payload = no shadow pushed yet; default to Engaged=false
	// (disengaged, normal interception) so the agent does not synthesize
	// a "killswitch engaged" state from missing data. The macOS NE proxy
	// bindings require fail-open at every layer.
	if len(raw) == 0 {
		return KillSwitchView{}
	}
	// Wire shape matches interception.Killswitch: {engaged: bool}.
	// Engaged=true = kill switch engaged (passthrough); Engaged=false =
	// normal interception. Pointer so an absent field defaults to false.
	var wrapped struct {
		Engaged *bool  `json:"engaged"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return KillSwitchView{}
	}
	engaged := false
	if wrapped.Engaged != nil {
		engaged = *wrapped.Engaged
	}
	return KillSwitchView{Engaged: engaged, Reason: wrapped.Reason}
}

func parseDeviceDefaults(raw json.RawMessage) DeviceDefaultsView {
	if len(raw) == 0 {
		return DeviceDefaultsView{}
	}
	var wrapped struct {
		QuitAllowed              *bool             `json:"quitAllowed"`
		HeartbeatIntervalSec     int               `json:"heartbeatIntervalSec"`
		AuditDrainIntervalSec    int               `json:"auditDrainIntervalSec"`
		ConfigSyncIntervalSec    int               `json:"configSyncIntervalSec"`
		AuditBatchSize           int               `json:"auditBatchSize"`
		ShutdownWarningEnabled   bool              `json:"shutdownWarningEnabled"`
		ShutdownWarning          map[string]string `json:"shutdownWarning"`
		AutoUpdateEnabled        bool              `json:"autoUpdateEnabled"`
		AutoUpdateChannel        string            `json:"autoUpdateChannel"`
		LogLevel                 string            `json:"logLevel"`
		TrafficUploadLevel       string            `json:"trafficUploadLevel"`
		ThemeID                  string            `json:"themeId"`
		ForceQUICFallbackBundles []string          `json:"forceQUICFallbackBundles"`
		AttestationEnabled       bool              `json:"attestationEnabled"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return DeviceDefaultsView{}
	}
	return DeviceDefaultsView{
		QuitAllowed:              wrapped.QuitAllowed,
		HeartbeatIntervalSec:     wrapped.HeartbeatIntervalSec,
		AuditDrainIntervalSec:    wrapped.AuditDrainIntervalSec,
		ConfigSyncIntervalSec:    wrapped.ConfigSyncIntervalSec,
		AuditBatchSize:           wrapped.AuditBatchSize,
		ShutdownWarningEnabled:   wrapped.ShutdownWarningEnabled,
		ShutdownWarning:          wrapped.ShutdownWarning,
		AutoUpdateEnabled:        wrapped.AutoUpdateEnabled,
		AutoUpdateChannel:        wrapped.AutoUpdateChannel,
		LogLevel:                 wrapped.LogLevel,
		TrafficUploadLevel:       wrapped.TrafficUploadLevel,
		ThemeID:                  wrapped.ThemeID,
		ForceQUICFallbackBundles: wrapped.ForceQUICFallbackBundles,
		AttestationEnabled:       wrapped.AttestationEnabled,
	}
}

// parseRulePacks reads the installed_rule_packs Cat B payload Hub serves
// via packages/nexus-hub/internal/storage/store/catb_agent_installed_rule_packs.go.
// Wire shape: {"installedRulePacks": [{id, packId, name, version,
// maintainer, description, boundHookId, enabled, ruleCount, installedAt}]}.
func parseRulePacks(raw json.RawMessage) []RulePackView {
	out := []RulePackView{}
	if len(raw) == 0 {
		return out
	}
	var wrapped struct {
		InstalledRulePacks []RulePackView `json:"installedRulePacks"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return out
	}
	return append(out, wrapped.InstalledRulePacks...)
}

// parseUserContext reads the user_context Cat B payload Hub serves via
// packages/nexus-hub/internal/storage/store/catb_agent_user_context.go. Returns
// {nil, nil} when the agent has no current user assignment — the UI
// then renders the "Sign in to see your identity context" empty state.
func parseUserContext(raw json.RawMessage) (*UserContextView, []OrganizationView) {
	if len(raw) == 0 {
		return nil, nil
	}
	var wrapped struct {
		User          *UserContextView   `json:"user"`
		Organizations []OrganizationView `json:"organizations"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, nil
	}
	return wrapped.User, wrapped.Organizations
}

func parseDiagMode(raw json.RawMessage) *DiagModeView {
	if len(raw) == 0 {
		return nil
	}
	var wrapped struct {
		DiagMode struct {
			Active bool   `json:"active"`
			Until  string `json:"until"`
		} `json:"diagMode"`
		DiagModeUntil string `json:"diagModeUntil"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil
	}
	// Two shapes today — admin may emit either { diagMode: { until }}
	// or a flat diagModeUntil at the agent_settings root. Treat
	// presence as "on" so future server-side reformatting doesn't
	// silently hide an admin-enabled window.
	if wrapped.DiagMode.Until != "" || wrapped.DiagMode.Active {
		return &DiagModeView{Active: true, Until: wrapped.DiagMode.Until}
	}
	if wrapped.DiagModeUntil != "" {
		return &DiagModeView{Active: true, Until: wrapped.DiagModeUntil}
	}
	return nil
}
