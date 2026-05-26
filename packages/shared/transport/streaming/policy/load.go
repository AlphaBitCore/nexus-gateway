package policy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// SystemMetadataKey is the system_metadata row that stores the global
// streaming compliance default. Each data-plane service reads it at
// startup and refreshes it from the same row on every
// `streaming_compliance` shadow invalidation.
const SystemMetadataKey = "streaming_compliance.config"

// rawConfig mirrors the JSON shape persisted under SystemMetadataKey. The
// admin UI / CP API write the same shape so an operator changing the
// config from a fresh write produces a cleanly reloadable Policy.
type rawConfig struct {
	DefaultMode         string `json:"default_mode,omitempty"`
	ChunkBytes          int    `json:"chunk_bytes,omitempty"`
	HookTimeoutMs       int    `json:"hook_timeout_ms,omitempty"`
	MaxBufferBytes      int    `json:"max_buffer_bytes,omitempty"`
	FailBehavior        string `json:"fail_behavior,omitempty"`
	CaptureRequestBody  *bool  `json:"capture_request_body,omitempty"`
	CaptureResponseBody *bool  `json:"capture_response_body,omitempty"`
	RawSpillEnabled     *bool  `json:"raw_body_spill_enabled,omitempty"`
}

// LoadGlobalDefault reads SystemMetadataKey from the system_metadata
// table and returns the materialized Policy. A missing row falls back
// to DefaultPolicy() so a fresh deployment runs with the conservative
// "no capture, passthrough" baseline rather than panicking.
//
// The query is intentionally thin so the interesting policy-resolution
// logic lives in policyFromQueryResult, which is unit-tested directly
// (LoadGlobalDefault itself remains an integration-tier surface).
func LoadGlobalDefault(ctx context.Context, db *sql.DB) (Policy, error) {
	var raw json.RawMessage
	err := db.QueryRowContext(ctx,
		`SELECT value FROM system_metadata WHERE key = $1`,
		SystemMetadataKey,
	).Scan(&raw)
	return policyFromQueryResult(raw, err)
}

// policyFromQueryResult applies the three-way decision (missing row →
// default, transient err → wrapped err + default, success → decoded
// policy) over a query outcome. Split out so unit tests can drive every
// branch without a real *sql.DB.
func policyFromQueryResult(raw json.RawMessage, queryErr error) (Policy, error) {
	if errors.Is(queryErr, sql.ErrNoRows) {
		return DefaultPolicy(), nil
	}
	if queryErr != nil {
		return DefaultPolicy(), fmt.Errorf("policy.LoadGlobalDefault: %w", queryErr)
	}
	return DecodeGlobalPolicy(raw)
}

// DecodeGlobalPolicy parses the JSON shape produced by the admin UI /
// stored under SystemMetadataKey into a Policy. Used by data-plane
// services that receive the blob via a config-push channel (agent's
// thingclient) instead of reading system_metadata directly. Empty input
// returns DefaultPolicy() so callers can pass nil/empty for "no override
// yet" without special-casing.
func DecodeGlobalPolicy(raw json.RawMessage) (Policy, error) {
	if len(raw) == 0 {
		return DefaultPolicy(), nil
	}
	var cfg rawConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return DefaultPolicy(), fmt.Errorf("policy.DecodeGlobalPolicy: parse: %w", err)
	}
	out := DefaultPolicy()
	if cfg.DefaultMode != "" {
		out.Mode = Mode(cfg.DefaultMode)
	}
	if cfg.ChunkBytes > 0 {
		out.ChunkBytes = cfg.ChunkBytes
	}
	if cfg.HookTimeoutMs > 0 {
		out.HookTimeoutMs = cfg.HookTimeoutMs
	}
	if cfg.MaxBufferBytes > 0 {
		out.MaxBufferBytes = cfg.MaxBufferBytes
	}
	if cfg.FailBehavior != "" {
		out.FailBehavior = FailBehavior(cfg.FailBehavior)
	}
	if cfg.CaptureRequestBody != nil {
		out.CaptureRequestBody = *cfg.CaptureRequestBody
	}
	if cfg.CaptureResponseBody != nil {
		out.CaptureResponseBody = *cfg.CaptureResponseBody
	}
	if cfg.RawSpillEnabled != nil {
		out.RawSpillEnabled = *cfg.RawSpillEnabled
	}
	if !out.IsValid() {
		return DefaultPolicy(), fmt.Errorf("policy.DecodeGlobalPolicy: row %q is invalid: %+v", SystemMetadataKey, out)
	}
	return out, nil
}

// OverrideFromColumns assembles an Override from the eight nullable
// per-resource columns. Values that fail validation (mode not one of
// the three enum literals, FailBehavior similar) are dropped silently
// so a single bad row cannot poison every host's policy — the resolver
// falls back to the global default for the affected field.
func OverrideFromColumns(
	mode *string,
	chunkBytes *int,
	hookTimeoutMs *int,
	maxBufferBytes *int,
	failBehavior *string,
	captureRequestBody *bool,
	captureResponseBody *bool,
	rawSpillEnabled *bool,
) *Override {
	o := &Override{}
	any := false
	if mode != nil {
		m := Mode(*mode)
		switch m {
		case ModePassThrough, ModeBufferFullBlock, ModeChunkedAsync:
			o.Mode = &m
			any = true
		}
	}
	if chunkBytes != nil && *chunkBytes >= 0 {
		v := *chunkBytes
		o.ChunkBytes = &v
		any = true
	}
	if hookTimeoutMs != nil && *hookTimeoutMs >= 0 {
		v := *hookTimeoutMs
		o.HookTimeoutMs = &v
		any = true
	}
	if maxBufferBytes != nil && *maxBufferBytes >= 0 {
		v := *maxBufferBytes
		o.MaxBufferBytes = &v
		any = true
	}
	if failBehavior != nil {
		fb := FailBehavior(*failBehavior)
		switch fb {
		case FailOpen, FailClose:
			o.FailBehavior = &fb
			any = true
		}
	}
	if captureRequestBody != nil {
		v := *captureRequestBody
		o.CaptureRequestBody = &v
		any = true
	}
	if captureResponseBody != nil {
		v := *captureResponseBody
		o.CaptureResponseBody = &v
		any = true
	}
	if rawSpillEnabled != nil {
		v := *rawSpillEnabled
		o.RawSpillEnabled = &v
		any = true
	}
	if !any {
		return nil
	}
	return o
}
