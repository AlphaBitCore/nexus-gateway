package catbagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// AgentPayloadCaptureLoader aggregates the admin-editable payload-capture
// config for the agent by reading system_metadata["payload_capture.config"].
//
// Shape returned by Load matches shared/payloadcapture.Store.ApplyShadowState:
//
//	{
//	  "storeRequestBody":   bool,
//	  "storeResponseBody":  bool,
//	  "maxInlineBodyBytes": int,
//	  "maxRequestBytes":    int,
//	  "maxResponseBytes":   int
//	}
//
// The system_metadata row is a single JSONB blob owned by
// control-plane/internal/handler/admin_extras.go::UpdatePayloadCaptureConfig.
// The SELECT here duplicates the DB.GetSystemMetadata surface to avoid a
// cp -> hub dependency cycle — same rationale as the other Cat B loaders.
// Any schema change to system_metadata must update both sites.
//
// thingID is accepted for interface uniformity with CatBLoader but is not
// yet used — per-device payload-capture overrides are a follow-up; today's
// config is global.
type AgentPayloadCaptureLoader struct {
	db     pgxQuerier
	logger *slog.Logger
}

// NewAgentPayloadCaptureLoader constructs a loader bound to the given pool.
// The logger is optional; when nil the loader does not emit warnings.
func NewAgentPayloadCaptureLoader(db pgxQuerier, logger *slog.Logger) *AgentPayloadCaptureLoader {
	return &AgentPayloadCaptureLoader{db: db, logger: logger}
}

// payloadCaptureConfigKey is the system_metadata row that stores the
// admin-editable payload capture config. Mirrors the constant in
// packages/control-plane/internal/handler/admin_extras.go and the key
// used by packages/compliance-proxy/internal/configloader/payloadcapture.go.
const payloadCaptureConfigKey = "payload_capture.config"

// agentPayloadCaptureWire is the JSON shape the agent's
// payloadcapture.Store expects. Tags mirror the system_metadata row
// written by admin_extras.go so a Load -> ApplyShadowState round-trip
// preserves semantics. Field order and tag names must stay aligned
// with shared/payloadcapture's DecodeConfigJSON wire format — Hub
// must surface every key the data-plane reducer recognises so the
// agent never has to fall back to per-field coercion.
type agentPayloadCaptureWire struct {
	StoreRequestBody   bool  `json:"storeRequestBody"`
	StoreResponseBody  bool  `json:"storeResponseBody"`
	MaxInlineBodyBytes int64 `json:"maxInlineBodyBytes"`
	MaxRequestBytes    int64 `json:"maxRequestBytes"`
	MaxResponseBytes   int64 `json:"maxResponseBytes"`
}

// Load reads system_metadata["payload_capture.config"] and returns it in
// the shape payloadcapture.Config marshals to. A missing row or a
// malformed JSON blob degrades gracefully to the zero-value default
// (capture flags off, 256 KiB inline cutoff, 10 MiB network caps) so a
// fresh deployment never crashes the agent reducer, never silently flips
// capture on, and never collapses the data-plane read caps.
//
// The version is unix(updated_at) of the system_metadata row; 0 when the
// row is absent. Bumping version on each admin edit lets the agent's
// shadow reducer treat the pull as idempotent.
func (l *AgentPayloadCaptureLoader) Load(ctx context.Context, _ string) (any, int64, error) {
	row := l.db.QueryRow(ctx,
		`SELECT value, updated_at FROM system_metadata WHERE key = $1`,
		payloadCaptureConfigKey,
	)

	var (
		raw       []byte
		updatedAt time.Time
	)
	if err := row.Scan(&raw, &updatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No admin edit yet — return the zero-value default.
			return defaultPayloadCaptureState(), 0, nil
		}
		return nil, 0, fmt.Errorf("catb: query system_metadata[%s]: %w",
			payloadCaptureConfigKey, err)
	}

	if len(raw) == 0 {
		return defaultPayloadCaptureState(), timestampVersion(updatedAt), nil
	}

	var wire agentPayloadCaptureWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		if l.logger != nil {
			l.logger.Warn("catb: system_metadata payload_capture.config malformed, using defaults",
				"error", err)
		}
		return defaultPayloadCaptureState(), timestampVersion(updatedAt), nil
	}
	if wire.MaxInlineBodyBytes <= 0 {
		wire.MaxInlineBodyBytes = payloadCaptureDefaultMaxInlineBodyBytes
	}
	if wire.MaxRequestBytes <= 0 {
		wire.MaxRequestBytes = payloadCaptureDefaultMaxRequestBytes
	}
	if wire.MaxResponseBytes <= 0 {
		wire.MaxResponseBytes = payloadCaptureDefaultMaxResponseBytes
	}

	return wire, timestampVersion(updatedAt), nil
}

// payloadCaptureDefault* mirror the constants in shared/payloadcapture.
// Duplicated (not imported) to keep the Cat B loader's runtime
// dependency surface minimal — Hub already pulls shared packages on the
// build path but avoiding the import here matches the pattern used by
// catb_agent_hook_config.go and friends. If shared/payloadcapture
// changes a default, this file must change in the same commit; the
// loader unit tests pin the byte values to flag any drift.
const (
	payloadCaptureDefaultMaxInlineBodyBytes int64 = 256 * 1024       // 256 KiB
	payloadCaptureDefaultMaxRequestBytes    int64 = 10 * 1024 * 1024 // 10 MiB
	payloadCaptureDefaultMaxResponseBytes   int64 = 10 * 1024 * 1024 // 10 MiB
)

// defaultPayloadCaptureState returns the zero-risk default aggregated
// state — both capture flags disabled, inline-vs-spill cutoff at 256 KiB
// (matching Postgres' efficient JSONB inline range), and 10 MiB network
// caps so the agent never silently truncates a forwarded body just
// because no admin row exists. Used when the admin has never written a
// row and when a malformed row needs to degrade safely.
func defaultPayloadCaptureState() agentPayloadCaptureWire {
	return agentPayloadCaptureWire{
		StoreRequestBody:   false,
		StoreResponseBody:  false,
		MaxInlineBodyBytes: payloadCaptureDefaultMaxInlineBodyBytes,
		MaxRequestBytes:    payloadCaptureDefaultMaxRequestBytes,
		MaxResponseBytes:   payloadCaptureDefaultMaxResponseBytes,
	}
}
