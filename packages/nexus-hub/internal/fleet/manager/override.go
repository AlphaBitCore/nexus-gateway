package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/traffic/chain"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	configtypes "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
)

// Errors returned by SetOverride / ClearOverride. store.ErrNotFound is also
// surfaced unchanged for missing-thing / missing-override callsites so the
// admin handler can map both to HTTP 404.
var (
	// ErrTemplateMissing — no thing_config_template row exists for the
	// (thing.type, configKey) pair. We refuse to write an override for a key
	// that was never templated because thing.desired then has no baseline to
	// fall back to when the override is cleared.
	ErrTemplateMissing = errors.New("no template exists for this (type, config_key)")

	// ErrKeyNotOverridable — configKey is in the configtypes blacklist
	// (configtypes.IsOverridable returns false). Mirrors the CP admin handler
	// validator; checked again here so any future non-CP caller (CLI, internal
	// job) cannot bypass the policy.
	ErrKeyNotOverridable = errors.New("config key is in the non-overridable blacklist")
)

// breakGlassReasonPrefix is the literal prefix on `reason` that, per spec
// §4 / §9, flips emergency_override to true even on non-killswitch keys.
// Operators set this when running an override under an active incident so
// the audit trail can highlight break-glass entries.
const breakGlassReasonPrefix = "break-glass:"

// SetOverrideRequest is the manager-level input shape for setting / updating
// a per-Thing override. The CP admin handler builds this from its validated
// HTTP payload and passes it straight through — this struct is the authoritative
// boundary between the HTTP edge (which handles RBAC + TTL clamping) and the
// Hub-internal write pipeline (which handles persistence + push).
type SetOverrideRequest struct {
	ThingID   string
	ConfigKey string
	// State must already be a JSON object at the top level; the CP handler
	// validates the type and rejects non-object payloads. Stored as raw JSON
	// so we can pass it through to UpsertOverride without an extra encode hop.
	State json.RawMessage
	// SetBy identifies the actor (user id / email / system actor); written into
	// thing_config_override.set_by AND admin_audit_log.actor_id.
	SetBy string
	// Reason is optional. Reasons starting with "break-glass:" auto-set
	// emergency_override=true even when the key is not killswitch.
	Reason *string
	// ExpiresAt is optional. CP handler clamps to [5m, 30d] before passing.
	// nil → permanent override (must be cleared explicitly).
	ExpiresAt *time.Time
}

// SetOverride persists a per-Thing override + recomputes thing.desired + bumps
// thing.desired_ver + writes admin_audit_log + force-pushes the affected key.
//
// The DB writes (override row + desired recompute + audit row) all happen in a
// single transaction so a failure rolls back every side effect. The
// post-commit RePushConfigKey call is intentionally outside the tx — pushing
// state that may roll back is worse than dropping a push (the next drift cycle
// will re-converge).
//
// Returns ErrKeyNotOverridable when configKey is in the blacklist.
// Returns ErrTemplateMissing when (thing.type, configKey) has no template.
// Returns store.ErrNotFound when the Thing does not exist.
func (m *Manager) SetOverride(ctx context.Context, req SetOverrideRequest) (*store.ThingConfigOverride, error) {
	if req.ThingID == "" || req.ConfigKey == "" {
		return nil, fmt.Errorf("thingID and configKey are required")
	}
	if !configtypes.IsOverridable(req.ConfigKey) {
		return nil, ErrKeyNotOverridable
	}
	// NewOverrideState rejects empty bytes / non-object JSON; the resulting
	// errors propagate up so the Hub HTTP handler maps them to 400. CP also
	// pre-validates, so well-formed admin traffic never sees these.
	overrideState, err := store.NewOverrideState(req.State)
	if err != nil {
		return nil, err
	}

	thing, err := m.store.RegistryStore().GetThing(ctx, req.ThingID)
	if err != nil {
		return nil, err
	}

	tmpl, err := m.store.ConfigStore().GetConfigTemplate(ctx, thing.Type, req.ConfigKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrTemplateMissing
		}
		return nil, fmt.Errorf("get config template: %w", err)
	}
	templateVerAtSet := tmpl.Version

	emergencyOverride := req.ConfigKey == "killswitch"
	if req.Reason != nil && strings.HasPrefix(*req.Reason, breakGlassReasonPrefix) {
		emergencyOverride = true
	}

	override := store.ThingConfigOverride{
		ThingID:           req.ThingID,
		ConfigKey:         req.ConfigKey,
		State:             overrideState,
		TemplateVerAtSet:  templateVerAtSet,
		SetBy:             req.SetBy,
		Reason:            req.Reason,
		ExpiresAt:         req.ExpiresAt,
		EmergencyOverride: emergencyOverride,
	}

	pool := m.txPool()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := m.store.OverrideStore().UpsertOverride(ctx, tx, override); err != nil {
		return nil, fmt.Errorf("upsert override: %w", err)
	}

	merged, err := m.recomputeDesiredTx(ctx, tx, thing.ID, thing.Type)
	if err != nil {
		return nil, fmt.Errorf("recompute desired: %w", err)
	}
	if _, err := m.store.RegistryStore().WriteDesiredAndBumpVer(ctx, tx, thing.ID, merged); err != nil {
		return nil, fmt.Errorf("write desired: %w", err)
	}

	auditMeta := map[string]any{
		"thingId":           req.ThingID,
		"configKey":         req.ConfigKey,
		"newState":          overrideState, // marshals as the validated object via OverrideState.MarshalJSON
		"templateVerAtSet":  templateVerAtSet,
		"emergencyOverride": emergencyOverride,
	}
	if req.Reason != nil {
		auditMeta["reason"] = *req.Reason
	}
	if req.ExpiresAt != nil {
		auditMeta["expiresAt"] = req.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	if err := insertAdminAuditLog(ctx, tx, adminAuditEntry{
		ActorID:    req.SetBy,
		ActorLabel: req.SetBy,
		Action:     "thing_override_set",
		EntityType: "thing",
		EntityID:   req.ThingID,
		AfterState: auditMeta,
	}); err != nil {
		return nil, fmt.Errorf("insert audit log: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// Re-fetch so set_at reflects the DB-side NOW() value rather than zero.
	persisted, err := m.store.OverrideStore().GetOverride(ctx, req.ThingID, req.ConfigKey)
	if err != nil {
		// The row was written successfully — surface the error but don't
		// pretend the write failed. Push still runs below.
		m.logger.Warn("override committed but re-fetch failed",
			slog.String("event", "override_refetch_failed"),
			slog.String("thing_id", req.ThingID),
			slog.String("config_key", req.ConfigKey),
			slog.String("error", err.Error()),
		)
	}

	if pushErr := m.RePushConfigKey(ctx, req.ThingID, req.ConfigKey); pushErr != nil {
		// Push failure is intentionally non-fatal here — the override row
		// is committed and drift detection will re-converge — but it MUST
		// be visible: an audit-committed override the client never receives
		// looks like success without this log line. ErrNoDeliveryPath fires
		// when neither WS nor MQ could deliver; other errors come from
		// marshal/publish/write paths. We unify the surface as
		// override_push_failed so dashboards alert on either.
		m.logger.Warn("override set: post-commit push failed (drift detect will repair)",
			slog.String("event", "override_push_failed"),
			slog.String("operation", "override_set"),
			slog.String("thing_id", req.ThingID),
			slog.String("config_key", req.ConfigKey),
			slog.String("error", pushErr.Error()),
		)
	}

	m.logger.Info("override set",
		slog.String("event", "thing_override_set"),
		slog.String("thing_id", req.ThingID),
		slog.String("config_key", req.ConfigKey),
		slog.String("set_by", req.SetBy),
		slog.Bool("emergency_override", emergencyOverride),
		slog.Int64("template_ver_at_set", templateVerAtSet),
	)

	if persisted != nil {
		return persisted, nil
	}
	// Fallback: return what we tried to write.
	return &override, nil
}

// ClearOverride removes a per-Thing override row and reverts thing.desired's
// entry for that key to the template state, bumping desired_ver.
//
// `actor` is written to admin_audit_log.actor_id; the override-expiry job
// passes "system:override-expiry-job" so the audit trail distinguishes
// admin-driven from automatic clears.
//
// Returns store.ErrNotFound when no override exists for (thingID, configKey).
// Returns store.ErrNotFound from GetThing when the Thing itself is missing
// (handler will surface either as HTTP 404).
func (m *Manager) ClearOverride(ctx context.Context, thingID, configKey, actor string) error {
	if thingID == "" || configKey == "" {
		return fmt.Errorf("thingID and configKey are required")
	}

	thing, err := m.store.RegistryStore().GetThing(ctx, thingID)
	if err != nil {
		return err
	}

	// Capture the prior override state for the audit row before deletion;
	// without this the audit trail shows nothing about what was cleared.
	prior, err := m.store.OverrideStore().GetOverride(ctx, thingID, configKey)
	if err != nil {
		return err
	}

	pool := m.txPool()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	existed, err := m.store.OverrideStore().DeleteOverride(ctx, tx, thingID, configKey)
	if err != nil {
		return fmt.Errorf("delete override: %w", err)
	}
	if !existed {
		// Race: GetOverride saw a row but a parallel session deleted it
		// between the two queries. Surface the missing-row outcome.
		return store.ErrNotFound
	}

	merged, err := m.recomputeDesiredTx(ctx, tx, thingID, thing.Type)
	if err != nil {
		return fmt.Errorf("recompute desired: %w", err)
	}
	if _, err := m.store.RegistryStore().WriteDesiredAndBumpVer(ctx, tx, thingID, merged); err != nil {
		return fmt.Errorf("write desired: %w", err)
	}

	auditMeta := map[string]any{
		"thingId":           thingID,
		"configKey":         configKey,
		"priorState":        prior.State, // OverrideState.MarshalJSON emits the canonical object bytes
		"priorSetBy":        prior.SetBy,
		"priorSetAt":        prior.SetAt.UTC().Format(time.RFC3339Nano),
		"emergencyOverride": prior.EmergencyOverride,
	}
	if prior.Reason != nil {
		auditMeta["priorReason"] = *prior.Reason
	}
	if prior.ExpiresAt != nil {
		auditMeta["priorExpiresAt"] = prior.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	if err := insertAdminAuditLog(ctx, tx, adminAuditEntry{
		ActorID:     actor,
		ActorLabel:  actor,
		Action:      "thing_override_cleared",
		EntityType:  "thing",
		EntityID:    thingID,
		BeforeState: auditMeta,
	}); err != nil {
		return fmt.Errorf("insert audit log: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	if pushErr := m.RePushConfigKey(ctx, thingID, configKey); pushErr != nil {
		// Same logic as SetOverride: post-commit push is best-effort. We
		// intentionally do NOT change the function return type — failure
		// here surfaces only via this warn line; drift detection repairs.
		m.logger.Warn("override clear: post-commit push failed (drift detect will repair)",
			slog.String("event", "override_push_failed"),
			slog.String("operation", "override_clear"),
			slog.String("thing_id", thingID),
			slog.String("config_key", configKey),
			slog.String("error", pushErr.Error()),
		)
	}

	m.logger.Info("override cleared",
		slog.String("event", "thing_override_cleared"),
		slog.String("thing_id", thingID),
		slog.String("config_key", configKey),
		slog.String("actor", actor),
	)
	return nil
}

// recomputeDesiredTx rebuilds the merged desired map for a Thing as
// (template state for every templated key) ⊕ (override state where present).
//
// Both the template list and the override list are read inside the supplied
// transaction so the recompute sees the same snapshot the upstream upsert /
// delete just wrote. Reading overrides outside the tx would be wrong: the
// freshly-upserted / deleted row would not be visible until commit, and the
// merged state would reflect the pre-write world.
//
// The template SELECT runs FOR SHARE so a concurrent admin updating a
// template row for the same (type, config_key) cannot interleave between
// our read and our merge — without the lock, the recompute could write a
// "merged desired" that lags both the template (the template moved on
// after we read) and the override (the override row is still in this tx).
// Override path doesn't mutate templates, so FOR SHARE is sufficient and
// adds only a row-level shared lock for the small per-type set of rows.
//
// Override state is stored as raw JSON; we Unmarshal each value to `any` so
// the consolidated map round-trips cleanly through json.Marshal in
// WriteDesiredAndBumpVer — raw JSON values would otherwise be re-encoded as
// base64 strings.
func (m *Manager) recomputeDesiredTx(ctx context.Context, tx pgx.Tx, thingID, thingType string) (map[string]any, error) {
	// Templates: read via the tx with FOR SHARE so a concurrent template
	// update on the same (type, config_key) blocks until our tx commits.
	tmplRows, err := tx.Query(ctx, `
		SELECT config_key, state
		FROM thing_config_template
		WHERE type = $1
		FOR SHARE
	`, thingType)
	if err != nil {
		return nil, fmt.Errorf("query templates: %w", err)
	}
	templates := make(map[string]any)
	for tmplRows.Next() {
		var key string
		var stateRaw []byte
		if err := tmplRows.Scan(&key, &stateRaw); err != nil {
			tmplRows.Close()
			return nil, fmt.Errorf("scan template: %w", err)
		}
		var v any
		if err := json.Unmarshal(stateRaw, &v); err != nil {
			tmplRows.Close()
			return nil, fmt.Errorf("unmarshal template state for %s: %w", key, err)
		}
		templates[key] = v
	}
	tmplRows.Close()
	if err := tmplRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate templates: %w", err)
	}

	// Overrides: must be read inside the tx so the recompute sees the same
	// snapshot the just-committed upsert / delete produced.
	ovRows, err := tx.Query(ctx, `
		SELECT config_key, state
		FROM thing_config_override
		WHERE thing_id = $1
	`, thingID)
	if err != nil {
		return nil, fmt.Errorf("query overrides: %w", err)
	}
	overrides := make(map[string]any)
	for ovRows.Next() {
		var key string
		var stateRaw []byte
		if err := ovRows.Scan(&key, &stateRaw); err != nil {
			ovRows.Close()
			return nil, fmt.Errorf("scan override: %w", err)
		}
		var v any
		if err := json.Unmarshal(stateRaw, &v); err != nil {
			ovRows.Close()
			return nil, fmt.Errorf("unmarshal override state for %s: %w", key, err)
		}
		overrides[key] = v
	}
	ovRows.Close()
	if err := ovRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate overrides: %w", err)
	}

	merged := make(map[string]any, len(templates)+len(overrides))
	for k, v := range templates {
		merged[k] = v
	}
	// Override-only keys (override exists but template was deleted) are kept
	// visible so admins can still see + clear the orphan.
	for k, v := range overrides {
		merged[k] = v
	}
	return merged, nil
}

// adminAuditEntry mirrors the AdminAuditLog table column shape for the small
// number of Hub-side direct writes (override CRUD + override-expiry job). The
// CP-side audit path goes via MQ → consumer.AdminAuditWriter; Hub-side override
// writes happen synchronously inside the override tx so a rollback drops the
// audit row too — that requires a direct DB INSERT here.
type adminAuditEntry struct {
	ActorID     string
	ActorLabel  string
	ActorRole   string
	Action      string
	EntityType  string
	EntityID    string
	BeforeState any
	AfterState  any
}

// insertAdminAuditLog writes one row into AdminAuditLog inside the supplied
// transaction. Hub is the sole chain writer (F3): we call chain.NextHash
// here under the same advisory lock the MQ consumer uses, so the chain is
// consistent across both writer paths.
//
// The timestamp is generated in Go (UTC) rather than via NOW(): the chain
// helper hashes the int64 ms-epoch from the payload, and that same value
// must be the one persisted in the timestamp column. Letting NOW() pick a
// different (sub-millisecond) value would make VerifyChain reject the row.
func insertAdminAuditLog(ctx context.Context, tx pgx.Tx, e adminAuditEntry) error {
	var beforeJSON, afterJSON json.RawMessage
	if e.BeforeState != nil {
		b, err := json.Marshal(e.BeforeState)
		if err != nil {
			return fmt.Errorf("marshal beforeState: %w", err)
		}
		beforeJSON = b
	}
	if e.AfterState != nil {
		a, err := json.Marshal(e.AfterState)
		if err != nil {
			return fmt.Errorf("marshal afterState: %w", err)
		}
		afterJSON = a
	}

	now := time.Now().UTC()
	payload, err := chain.NewHashPayload(e.Action, e.ActorID, e.EntityType, e.EntityID)
	if err != nil {
		return fmt.Errorf("build hash payload: %w", err)
	}
	payload.TimestampMs = now.UnixMilli()
	payload.BeforeState = beforeJSON
	payload.AfterState = afterJSON
	prevHash, integrityHash, hashInput, err := chain.NextHash(ctx, tx, payload)
	if err != nil {
		return fmt.Errorf("compute chain hash: %w", err)
	}

	var prevArg any
	if prevHash != "" {
		prevArg = prevHash
	}

	id := uuid.New().String()
	if _, err := tx.Exec(ctx, `
		INSERT INTO "AdminAuditLog" (
			id, timestamp,
			"actorId", "actorLabel", "actorRole",
			action, "entityType", "entityId",
			"beforeState", "afterState",
			"previousHash", "integrityHash", "hashInput"
		) VALUES (
			$1, to_timestamp($2 / 1000.0),
			$3, $4, $5,
			$6, $7, $8,
			$9, $10,
			$11, $12, $13
		)
	`,
		id, payload.TimestampMs,
		e.ActorID, e.ActorLabel, nullableString(e.ActorRole),
		e.Action, e.EntityType, nullableString(e.EntityID),
		nullableJSON(beforeJSON), nullableJSON(afterJSON),
		prevArg, integrityHash, hashInput,
	); err != nil {
		return fmt.Errorf("insert AdminAuditLog: %w", err)
	}
	return nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
