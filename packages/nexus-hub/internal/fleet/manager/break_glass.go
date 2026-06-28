package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"sort"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/identity"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/interception"
)

// actorName is the actor_name / actor_label stamped on every break-glass audit
// row regardless of which token triggered it; the per-incident identity lives
// in actor_id ("break-glass:<tokenID>").
const actorName = "break-glass"

// breakGlassPerThingTTL is the auto-revert window stamped on a per-Thing
// break-glass override. A break-glass exemption is an emergency
// bypass, conceptually the agent-side twin of the AI Gateway's emergency
// passthrough, which carries a mandatory ExpiresAt ≤ 8h
// (kill-switch-architecture.md §9). Matching that 8h bound means a node's
// emergency exemption auto-reverts via the existing override-expiry job
// instead of lingering as a permanent silent bypass nobody remembers to clear.
// Only the per-Thing path gets a TTL: the fleet-scoped killswitch deliberately
// has no auto-revert timer (kill-switch-architecture.md §8 — the operator is on
// the hook for disengaging it once the incident is over).
const breakGlassPerThingTTL = 8 * time.Hour

// breakGlassWritableKeys is the SERVER-SIDE allowlist for break-glass
// reconciliation. It mirrors the data-plane client allowlist (the
// {killswitch, exemptions} switch in
// packages/compliance-proxy/internal/runtime/breakglass/break_glass.go
// applyBreakGlassLocal) — privilege MUST NOT be broader server-side than the
// only client that exercises it. Without this gate, a device/service
// token for type T could rewrite ANY thing_config_template[T,*] row fleet-wide
// (including security keys) via a hand-crafted break-glass report. Any key not
// in this set is rejected, never silently dropped.
var breakGlassWritableKeys = map[string]bool{
	configkey.Killswitch: true,
	configkey.Exemptions: true,
}

// A node-initiated break-glass report may NEVER write the
// fleet-wide thing_config_template — it can only override config on the reporting
// Thing itself (per-Thing). A node holds only its OWN device/service token; a
// fingerprint/token match is not operator authority, so letting one compromised
// node flip a FLEET-WIDE safety control (engage killswitch → drop the whole fleet
// into uninspected passthrough, or disengage an operator-set killswitch) is a
// cross-node blast radius an attacker abuses. Genuine fleet-wide killswitch stays
// EXCLUSIVELY on the operator admin endpoint POST /api/admin/compliance/killswitch
// (`admin:kill-switch.toggle` IAM verb). The single-node Hub-outage fallback is
// preserved: a node can still break-glass-engage killswitch ON ITSELF (per-Thing
// override, like exemptions). This DEVIATES from the earlier
// kill-switch-architecture.md §1 "node break-glass is fleet-wide" model — see the
// updated doc. No break-glass key is fleet-scoped any more, so every key adopts as
// a per-Thing override.

// ErrBreakGlassKeyNotAllowed is returned when a break-glass report carries a
// config key outside breakGlassWritableKeys.
var ErrBreakGlassKeyNotAllowed = errors.New("break-glass key not in writable allowlist")

// ErrBreakGlassStateInvalid is returned when a break-glass state payload does
// not decode cleanly into its key's canonical configtypes schema.
var ErrBreakGlassStateInvalid = errors.New("break-glass state failed schema validation")

// ValidateBreakGlassReport enforces the server-side break-glass authority gate.
// Every key in the report MUST be in the writable allowlist AND its
// reported state MUST decode cleanly into the key's canonical configtypes
// struct with no type mismatch and no unknown fields. A legitimate break-glass
// payload is always a clean marshal of the data plane's own typed struct, so it
// round-trips without loss; only hand-crafted / type-mismatched payloads fail.
//
// Returns the first violation. Callers at the HTTP edge use this to return a
// 400 before dispatch; handleBreakGlassReport re-checks it as the authoritative
// backstop for the WebSocket path (which has no response channel) and for any
// future caller that bypasses the edge.
func ValidateBreakGlassReport(req ShadowReportRequest) error {
	if len(req.KeyVersions) == 0 {
		return fmt.Errorf("break-glass report missing keyVersions")
	}
	for key := range req.KeyVersions {
		if !breakGlassWritableKeys[key] {
			return fmt.Errorf("%w: %q", ErrBreakGlassKeyNotAllowed, key)
		}
		state, ok := req.Reported[key]
		if !ok {
			// A key version declared with no reported state attached: not a
			// schema violation. Adoption skips it rather than overwriting the
			// template/override with a missing value; validate only what is
			// present here.
			continue
		}
		if err := validateBreakGlassState(key, state); err != nil {
			return err
		}
	}
	return nil
}

// validateBreakGlassState decodes state into the canonical configtypes struct
// for key with DisallowUnknownFields, so a malformed shape (wrong JSON type,
// or an unknown/typo field such as the legacy "enabled" instead of "engaged")
// is rejected before it can corrupt config parsing for the whole type.
func validateBreakGlassState(key string, state any) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("%w: marshal %s: %v", ErrBreakGlassStateInvalid, key, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	switch key {
	case configkey.Killswitch:
		var v interception.Killswitch
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrBreakGlassStateInvalid, key, err)
		}
	case configkey.Exemptions:
		var v identity.ActiveExemptions
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrBreakGlassStateInvalid, key, err)
		}
	default:
		// Unreachable in normal flow: callers gate on breakGlassWritableKeys
		// first. Kept closed so a future allowlist addition without a matching
		// schema case fails loud rather than silently skipping validation.
		return fmt.Errorf("%w: no schema registered for %q", ErrBreakGlassStateInvalid, key)
	}
	return nil
}

// handleBreakGlassReport reconciles a shadow_report produced by a data-plane
// break-glass handler. It is reached from HandleShadowReport when
// req.Reason == "break_glass" (set by the dedicated break-glass WS dispatch
// case and the /shadow/break-glass HTTP route — NOT by a body-supplied reason
// on the normal shadow path).
//
// Authority gate: the report is validated against the writable
// allowlist + per-key schema up front; ANY violation rejects the WHOLE report
// before any write, so a bad key can never produce a partial adoption.
//
// Scope routing (superseding the original fleet-wide/per-Thing split): EVERY
// node-initiated break-glass key — killswitch included — is adopted as a
// PER-THING desired override on the reporting Thing only, never the fleet-wide
// thing_config_template. So one node's emergency action can never become the
// type default for the whole fleet; the genuine fleet-wide kill-switch lives
// exclusively on the IAM-gated operator admin endpoint. Adoption is
// version-guarded: a stale report (an admin write already superseded the local
// override) is skipped.
func (m *Manager) handleBreakGlassReport(ctx context.Context, req ShadowReportRequest) error {
	// Authoritative server-side gate. The HTTP edge validates first to return
	// 400; this re-check is the backstop for the WS path and any other caller.
	if err := ValidateBreakGlassReport(req); err != nil {
		return err
	}

	thing, err := m.store.RegistryStore().GetThing(ctx, req.ID)
	if err != nil {
		return fmt.Errorf("get thing %s: %w", req.ID, err)
	}

	actorID := "break-glass:" + req.ActorTokenID
	if req.ActorTokenID == "" {
		actorID = "break-glass:unknown"
	}

	for key, reportedVer := range req.KeyVersions {
		state, ok := req.Reported[key]
		if !ok {
			// The key version was declared but no reported state was attached;
			// skip rather than overwriting with a missing value.
			continue
		}
		// Every node-initiated break-glass key (killswitch included)
		// adopts as a PER-THING override on the reporting Thing only — never the
		// fleet-wide template.
		if err := m.adoptBreakGlassPerThing(ctx, thing, key, state, reportedVer, actorID, req.SourceIP); err != nil {
			return err
		}
	}
	return nil
}

// emitBreakGlassDenied writes a durable break_glass_denied config-change event
// when a node's break-glass report is rejected by ValidateBreakGlassReport — a
// key outside the {killswitch, exemptions} writable allowlist or a schema-invalid
// state. Without this a rogue node can probe/fuzz the privileged
// break-glass reconciliation surface leaving only rotatable Hub Error logs,
// invisible to the audit/SIEM stream. Best-effort: any failure is logged and
// swallowed so recording a denial never changes the shadow-report outcome.
func (m *Manager) emitBreakGlassDenied(ctx context.Context, req ShadowReportRequest, denyErr error) {
	keys := make([]string, 0, len(req.KeyVersions))
	for k := range req.KeyVersions {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic for the audit row
	configKey := strings.Join(keys, ",")

	actorID := "break-glass:" + req.ActorTokenID
	if req.ActorTokenID == "" {
		actorID = "break-glass:unknown"
	}

	thingType := ""
	if thing, err := m.store.RegistryStore().GetThing(ctx, req.ID); err == nil && thing != nil {
		thingType = thing.Type
	}

	pool := m.txPool()
	tx, err := pool.Begin(ctx)
	if err != nil {
		m.logger.Error("break-glass denied: begin audit tx", "thing_id", req.ID, "err", err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if insErr := m.store.ConfigStore().InsertConfigChangeEvent(ctx, tx, store.ConfigChangeEvent{
		ThingType: thingType,
		ConfigKey: configKey,
		Action:    "break_glass_denied",
		ActorID:   actorID,
		ActorName: actorName,
		NewState: map[string]any{
			"thingId":       req.ID,
			"reason":        denyErr.Error(),
			"attemptedKeys": keys,
		},
		NewVersion:        req.ReportedVer,
		SourceIP:          req.SourceIP,
		EmergencyOverride: false,
	}); insErr != nil {
		m.logger.Error("break-glass denied: insert audit event", "thing_id", req.ID, "err", insErr)
		return
	}
	if cmErr := tx.Commit(ctx); cmErr != nil {
		m.logger.Error("break-glass denied: commit audit event", "thing_id", req.ID, "err", cmErr)
	}
}

// adoptBreakGlassPerThing adopts a non-fleet break-glass key (e.g. exemptions)
// as a PER-THING desired override on the reporting Thing only — never the
// fleet-wide template. The write takes AcquireConfigVersionLock
// like the other per-Thing override writers (SetOverride /
// ClearOverride) so the desired_ver bump cannot collide with the type-fanout
// path, then upserts the override (emergency_override=true), recomputes the
// merged desired, bumps desired_ver, and writes a chain-hashed admin audit row
// — all in one transaction. The adopted override is pushed to the live node
// post-commit (best-effort; drift detection repairs a dropped push).
//
// Stale-write guard: the data plane computes the reported version as
// max(desiredVer, reportedVer)+1, so a fresh emergency always exceeds the
// Thing's last-known desired_ver. A report whose version no longer exceeds the
// current desired_ver means an admin/Hub write already advanced this Thing past
// the emergency point and is skipped.
func (m *Manager) adoptBreakGlassPerThing(ctx context.Context, thing *store.Thing, key string, state any, reportedVer int64, actorID, sourceIP string) error {
	if reportedVer <= thing.DesiredVer {
		return nil
	}

	// The override layer needs a template baseline so desired can fall back to
	// the template state when the override is later cleared (mirrors
	// SetOverride's ErrTemplateMissing contract).
	tmpl, err := m.store.ConfigStore().GetConfigTemplate(ctx, thing.Type, key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("break-glass per-thing %s.%s: %w", thing.ID, key, ErrTemplateMissing)
		}
		return fmt.Errorf("break-glass per-thing get template %s.%s: %w", thing.ID, key, err)
	}

	stateBytes, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("break-glass per-thing marshal %s.%s: %w", thing.ID, key, err)
	}
	overrideState, err := store.NewOverrideState(stateBytes)
	if err != nil {
		return fmt.Errorf("break-glass per-thing state %s.%s: %w", thing.ID, key, err)
	}

	reason := actorID
	// Auto-revert window: stamp expires_at so the override-expiry job
	// reverts this emergency exemption after breakGlassPerThingTTL instead of
	// leaving it as a permanent silent bypass. Computed Hub-side from the
	// adoption time, not trusted from the report.
	expiresAt := time.Now().UTC().Add(breakGlassPerThingTTL)
	override := store.ThingConfigOverride{
		ThingID:           thing.ID,
		ConfigKey:         key,
		State:             overrideState,
		TemplateVerAtSet:  tmpl.Version,
		SetBy:             actorID,
		Reason:            &reason,
		ExpiresAt:         &expiresAt,
		EmergencyOverride: true,
	}

	pool := m.txPool()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("break-glass per-thing begin %s.%s: %w", thing.ID, key, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Serialize the per-type desired_ver bump against UpdateConfig and
	// the admin override writers. MUST be the first statement in the tx for
	// consistent lock ordering. See store.AcquireConfigVersionLock.
	if err := m.store.RegistryStore().AcquireConfigVersionLock(ctx, tx, thing.Type); err != nil {
		return fmt.Errorf("break-glass per-thing acquire lock %s: %w", thing.Type, err)
	}
	if err := m.store.OverrideStore().UpsertOverride(ctx, tx, override); err != nil {
		return fmt.Errorf("break-glass per-thing upsert override %s.%s: %w", thing.ID, key, err)
	}
	merged, err := m.recomputeDesiredTx(ctx, tx, thing.ID, thing.Type)
	if err != nil {
		return fmt.Errorf("break-glass per-thing recompute %s: %w", thing.ID, err)
	}
	newVer, err := m.store.RegistryStore().WriteDesiredAndBumpVer(ctx, tx, thing.ID, merged)
	if err != nil {
		return fmt.Errorf("break-glass per-thing write desired %s: %w", thing.ID, err)
	}

	auditMeta := map[string]any{
		"thingId":           thing.ID,
		"configKey":         key,
		"newState":          state,
		"newVersion":        newVer,
		"templateVerAtSet":  tmpl.Version,
		"emergencyOverride": true,
		"reason":            reason,
		"sourceIp":          sourceIP,
		"expiresAt":         expiresAt.Format(time.RFC3339Nano),
	}
	if err := insertAdminAuditLog(ctx, tx, adminAuditEntry{
		ActorID:    actorID,
		ActorLabel: actorName,
		Action:     "thing_override_set",
		EntityType: "thing",
		EntityID:   thing.ID,
		AfterState: auditMeta,
	}); err != nil {
		return fmt.Errorf("break-glass per-thing insert audit %s.%s: %w", thing.ID, key, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("break-glass per-thing commit %s.%s: %w", thing.ID, key, err)
	}

	// Post-commit push so the adopting node receives its own emergency override
	// back as authoritative desired. Best-effort: drift detection re-converges
	// a dropped push, so a failure here is logged, not fatal.
	if pushErr := m.RePushConfigKey(ctx, thing.ID, key); pushErr != nil {
		m.logger.Warn("break-glass per-thing: post-commit push failed (drift detect will repair)",
			"thing_id", thing.ID, "config_key", key, "error", pushErr)
	}
	return nil
}
