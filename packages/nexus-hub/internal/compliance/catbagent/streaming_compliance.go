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

// AgentStreamingComplianceLoader serves the admin-editable streaming
// compliance config (mode / chunk_bytes / hook_timeout_ms /
// max_buffer_bytes / fail_behavior / capture flags / raw spill flag)
// to agents via the standard Cat B shadow channel.
//
// Wire shape mirrors shared/streaming/policy.DecodeGlobalPolicy
// (snake_case keys to match the JSON the admin UI writes via
// control-plane/internal/handler/admin_extras.go::UpdateStreamingPolicyConfig
// — same pattern as payload_capture). The agent's
// shared/streaming/policy.Store.ApplyShadowState consumes this directly.
//
// Without this loader the Hub returned an empty payload (4-byte "null")
// to the agent's payload pull, so admin's Streaming Compliance
// settings (mode chunked_async, hook_timeout_ms 2000, capture toggles
// etc.) silently never reached the agent runtime — every bumped flow
// fell back to DefaultPolicy (passthrough mode, no body capture).
type AgentStreamingComplianceLoader struct {
	db     pgxQuerier
	logger *slog.Logger
}

// NewAgentStreamingComplianceLoader constructs a loader bound to the
// supplied pgx pool.
func NewAgentStreamingComplianceLoader(db pgxQuerier, logger *slog.Logger) *AgentStreamingComplianceLoader {
	return &AgentStreamingComplianceLoader{db: db, logger: logger}
}

// streamingComplianceConfigKey is the system_metadata row that stores
// the admin-editable streaming compliance config. Mirrors the constant
// in shared/streaming/policy.SystemMetadataKey.
const streamingComplianceConfigKey = "streaming_compliance.config"

// agentStreamingComplianceWire is the JSON shape the agent's
// streampolicy.Store expects. Tags mirror the system_metadata row +
// shared/streaming/policy.rawConfig for byte-perfect parity. Pointers
// for the bool fields preserve the "field absent — inherit default"
// semantic that DecodeGlobalPolicy relies on; raw bool values would
// silently overwrite the agent's runtime default with `false` even
// when the admin meant "leave alone".
type agentStreamingComplianceWire struct {
	DefaultMode         string `json:"default_mode,omitempty"`
	ChunkBytes          int    `json:"chunk_bytes,omitempty"`
	HookTimeoutMs       int    `json:"hook_timeout_ms,omitempty"`
	MaxBufferBytes      int    `json:"max_buffer_bytes,omitempty"`
	FailBehavior        string `json:"fail_behavior,omitempty"`
	CaptureRequestBody  *bool  `json:"capture_request_body,omitempty"`
	CaptureResponseBody *bool  `json:"capture_response_body,omitempty"`
	RawSpillEnabled     *bool  `json:"raw_body_spill_enabled,omitempty"`
}

// Load reads system_metadata["streaming_compliance.config"] and returns
// the JSON shape DecodeGlobalPolicy on the agent side recognises.
// Missing row / malformed JSON degrades to an empty wire object so
// the agent applies DefaultPolicy (conservative passthrough). Version
// is unix(updated_at) for the agent's idempotent shadow apply.
func (l *AgentStreamingComplianceLoader) Load(ctx context.Context, _ string) (any, int64, error) {
	row := l.db.QueryRow(ctx,
		`SELECT value, updated_at FROM system_metadata WHERE key = $1`,
		streamingComplianceConfigKey,
	)
	var (
		raw       []byte
		updatedAt time.Time
	)
	if err := row.Scan(&raw, &updatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return agentStreamingComplianceWire{}, 0, nil
		}
		return nil, 0, fmt.Errorf("catb: query system_metadata[%s]: %w",
			streamingComplianceConfigKey, err)
	}
	if len(raw) == 0 {
		return agentStreamingComplianceWire{}, timestampVersion(updatedAt), nil
	}
	var wire agentStreamingComplianceWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		if l.logger != nil {
			l.logger.Warn("catb: system_metadata streaming_compliance.config malformed, using defaults",
				"error", err)
		}
		return agentStreamingComplianceWire{}, timestampVersion(updatedAt), nil
	}
	return wire, timestampVersion(updatedAt), nil
}
