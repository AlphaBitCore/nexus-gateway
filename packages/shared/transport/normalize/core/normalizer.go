package core

import (
	"context"
	"errors"
)

// SchemaVersion is the value stored in NormalizedPayload.NormalizeVersion
// and traffic_event_normalized.normalize_version. Bumped only on a
// backward-incompatible payload shape change.
const SchemaVersion = "1"

// ErrUnsupported is returned by a Normalizer when its input does not
// match the protocol / content-type this normalizer handles. Callers
// (the NormalizerRegistry) treat this as "try the next normalizer";
// hooks downstream see traffic_event_normalized.*_status="failed" only
// when every normalizer in the registry chain returns ErrUnsupported
// or its equivalent.
var ErrUnsupported = errors.New("normalize: input not supported by this normalizer")

// Meta carries adapter-supplied context about the captured bytes.
// Producers fill this from the request/response envelope they hold;
// Normalizer implementations read selectively (e.g. ContentType for the
// generic-http normalizer, AdapterType for AI normalizers).
type Meta struct {
	// AdapterType is the wire-format key chosen by routing
	// (e.g. "openai", "anthropic", "gemini", "vertex", "bedrock").
	// This is the registry-lookup key — operator-named provider strings
	// like "groq-east" or "google-gemini-prod" are intentionally NOT
	// used here; the routing layer resolves provider → adapter type
	// before invoking normalize. Empty for non-AI traffic captured by
	// compliance-proxy or agent.
	AdapterType string

	// Model identifier as reported by the request body or routing layer.
	Model string

	// ContentType is the request/response Content-Type header without
	// parameters (e.g. "application/json", not "application/json; charset=utf-8").
	ContentType string

	// Direction tells the normalizer whether it is producing the
	// request side or the response side of the exchange.
	Direction Direction

	// EndpointPath is the captured HTTP path (e.g. "/v1/chat/completions").
	// AI normalizers use this to route between chat / completion /
	// embedding / image endpoints under the same provider.
	EndpointPath string

	// Stream is true when the captured bytes represent a streaming
	// response (SSE, chunked event stream). Normalizers fold chunks
	// into the final assembled payload.
	Stream bool

	// SpillRef, when non-nil, addresses the original raw bytes in the
	// spill store; AI image / binary content references this rather
	// than inlining bytes into the normalized JSON.
	SpillRef *BinaryRef
}

// Normalizer transforms raw captured bytes into a NormalizedPayload.
//
// Implementations must be:
//   - Pure functions of (raw, meta) — no DB, no network, no logging at
//     LevelInfo or higher.
//   - Deterministic — same input yields byte-identical output across
//     the three data-plane services.
//   - Non-mutating — raw input is treated as immutable.
//
// On a partial parse, the normalizer SHOULD return a NormalizedPayload
// populated as far as it could go, paired with a wrapped error whose
// chain includes ErrUnsupported when the protocol mismatched, or a
// fresh error when the protocol matched but the payload was malformed.
// The pipeline records "partial" or "failed" status based on whether
// the returned payload is usable.
type Normalizer interface {
	// ID returns a stable identifier used in metrics labels and logs.
	// Format: "<protocol>-<direction-optional>"
	// Examples: "openai-chat", "anthropic-messages", "gemini-generate",
	// "generic-http".
	ID() string

	// Normalize produces a NormalizedPayload from raw bytes.
	Normalize(ctx context.Context, raw []byte, meta Meta) (NormalizedPayload, error)
}
