package manager

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// ShadowReportRequest is the input for a shadow report.
type ShadowReportRequest struct {
	ID          string         `json:"id"`
	Reported    map[string]any `json:"reported"`
	ReportedVer int64          `json:"reportedVer"`

	// KeyVersions carries per-key version numbers when the report was produced
	// by a break-glass handler that bumped a single key. Nil for normal reports.
	KeyVersions map[string]int64 `json:"keyVersions,omitempty"`

	// Reason is "" for normal reports, "break_glass" for emergency interventions
	// that require Hub-side reconciliation into thing_config_template.
	Reason string `json:"reason,omitempty"`

	// SourceIP is filled by the break-glass data-plane side and echoed into
	// config_change_event.source_ip for ops forensics.
	SourceIP string `json:"sourceIp,omitempty"`

	// ActorTokenID is the 8-char sha256 prefix of the elevated bearer token.
	// Used as suffix in actor_id, e.g. "break-glass:a1b2c3d4".
	ActorTokenID string `json:"actorTokenId,omitempty"`

	// ReportedOutcomesRaw is the per-key ApplyOutcome ledger the Thing's
	// OutcomeTracker emits inside every shadow_report. Wire-typed as raw
	// JSON because the ws layer is intentionally orthogonal to the store
	// wire shape — decoding into store.ReportedKeyOutcome happens here.
	// nil/empty is allowed: older Things and break-glass paths won't
	// populate it, and Hub treats missing as "no change" rather than
	// "wipe the ledger" (see UpdateShadowReport).
	ReportedOutcomesRaw map[string]json.RawMessage `json:"reportedOutcomes,omitempty"`
}

// decodeOutcomes decodes the raw outcome ledger into the typed store form.
// Bad/unknown entries are dropped with a warning rather than failing the
// whole report — operators would rather see N-1 keys than lose the entire
// report because one key's payload was malformed.
func decodeOutcomes(raw map[string]json.RawMessage) map[string]store.ReportedKeyOutcome {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]store.ReportedKeyOutcome, len(raw))
	for k, payload := range raw {
		var o store.ReportedKeyOutcome
		if err := json.Unmarshal(payload, &o); err != nil {
			continue
		}
		out[k] = o
	}
	return out
}

// HandleShadowReport processes a Thing's reported config state.
func (m *Manager) HandleShadowReport(ctx context.Context, req ShadowReportRequest) error {
	reportedKeys := 0
	if req.Reported != nil {
		reportedKeys = len(req.Reported)
	}
	m.logger.Info("shadow report received",
		"event", "shadow_report_received",
		"thing_id", req.ID,
		"reported_ver", req.ReportedVer,
		"reason", req.Reason,
		"reported_keys", reportedKeys)

	outcomes := decodeOutcomes(req.ReportedOutcomesRaw)
	err := m.store.RegistryStore().UpdateShadowReport(ctx, req.ID, req.Reported, req.ReportedVer, outcomes)
	if err != nil {
		return fmt.Errorf("shadow report %s: %w", req.ID, err)
	}

	// Cache shadow in Redis
	m.cacheShadow(ctx, req.ID, req.Reported)

	// Break-glass reconciliation: when a data-plane Thing performs an
	// emergency override of a config value, it marks the report with
	// Reason="break_glass" and carries per-key versions. The Hub must adopt
	// that state into thing_config_template and emit an emergency_override
	// audit event. Reconciliation failures are non-fatal: shadow_state is
	// already updated, and the data plane does not retry on errors returned
	// here (which would risk retry storms).
	if req.Reason == "break_glass" {
		if bgErr := m.handleBreakGlassReport(ctx, req); bgErr != nil {
			m.logger.Error("break-glass reconciliation", "thing_id", req.ID, "err", bgErr)
			// A rejected break-glass (a key outside the writable
			// allowlist or a schema-invalid state) is a node attempting a
			// disallowed privileged config write — it MUST leave a durable
			// security event, not just a rotatable Hub log line, so a rogue node
			// probing/fuzzing the break-glass authority surface is visible to the
			// audit/SIEM stream. Best-effort: failure to record must not change
			// the shadow-report outcome.
			m.emitBreakGlassDenied(ctx, req, bgErr)
		}
	}

	m.logger.Info("shadow report accepted",
		"thing_id", req.ID, "reported_ver", req.ReportedVer, "reason", req.Reason, "reported_keys", reportedKeys)
	return nil
}

// ShadowComparison is the per-key desired vs reported comparison.
type ShadowComparison struct {
	ThingID     string                   `json:"thingId"`
	ThingType   string                   `json:"thingType"`
	DesiredVer  int64                    `json:"desiredVer"`
	ReportedVer int64                    `json:"reportedVer"`
	Synced      bool                     `json:"synced"`
	Keys        map[string]ShadowKeyDiff `json:"keys"`
}

// ShadowKeyDiff shows desired vs reported for a single config key.
type ShadowKeyDiff struct {
	Desired  any  `json:"desired"`
	Reported any  `json:"reported"`
	Synced   bool `json:"synced"`
	// InDesired is true when this key is present in thing.desired. It is false
	// for a "reported-only" key — one the Thing still reports but the Hub no
	// longer desires (desired keys are removed on template-unassign / override
	// removal, but the reported map is merge-only and never prunes; see
	// UpdateShadowReport in fleet/store). The admin shadow view still renders
	// such a key as unsynced (operator-useful), but the content-drift
	// auto-heal MUST ignore it: ForceResyncAll only pushes desired keys, so a
	// reported-only key can never be reconciled and would loop forever.
	InDesired bool `json:"inDesired"`
}

// GetShadowComparison builds a per-key desired vs reported comparison.
func (m *Manager) GetShadowComparison(ctx context.Context, id string) (*ShadowComparison, error) {
	thing, err := m.store.RegistryStore().GetThing(ctx, id)
	if err != nil {
		return nil, err
	}

	keys := make(map[string]ShadowKeyDiff)
	allKeys := map[string]struct{}{}
	for k := range thing.Desired {
		allKeys[k] = struct{}{}
	}
	for k := range thing.Reported {
		allKeys[k] = struct{}{}
	}
	for k := range allKeys {
		d := thing.Desired[k]
		r := thing.Reported[k]
		_, inDesired := thing.Desired[k]
		dj, _ := json.Marshal(d)
		rj, _ := json.Marshal(r)
		keys[k] = ShadowKeyDiff{
			Desired:   d,
			Reported:  r,
			Synced:    string(dj) == string(rj),
			InDesired: inDesired,
		}
	}

	return &ShadowComparison{
		ThingID:     thing.ID,
		ThingType:   thing.Type,
		DesiredVer:  thing.DesiredVer,
		ReportedVer: thing.ReportedVer,
		Synced:      thing.ReportedVer >= thing.DesiredVer,
		Keys:        keys,
	}, nil
}

func (m *Manager) cacheShadow(ctx context.Context, thingID string, reported map[string]any) {
	if m.redis == nil {
		return
	}
	for key, val := range reported {
		data, err := json.Marshal(val)
		if err != nil {
			continue
		}
		rkey := fmt.Sprintf("nexus:shadow:%s:%s", thingID, key)
		m.redis.Set(ctx, rkey, data, 1*time.Hour)
	}
}
