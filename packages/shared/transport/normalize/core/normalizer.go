package core

import (
	"context"
	"errors"
)

// SchemaVersion is the value stored in NormalizedPayload.NormalizeVersion
// and traffic_event_normalized.normalize_version. Bumping it makes the
// hub's normalize backfill treat every row stamped with an older version
// as a re-normalization candidate, so historical rows heal automatically
// — no data migration. Version 2: decoder-unification payload shape
// (stream-fold content blocks, http-sse frame projections, frame-coverage
// confidence, fallback provenance stamps).
const SchemaVersion = "2"

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

// Sniffer is an optional Normalizer capability. A codec that can cheaply
// recognise its own wire shape from leading bytes implements LooksLike;
// the Registry offers it traffic whose AdapterType/path keys did not
// resolve (capture-side events routinely carry a host or tool name in
// AdapterType rather than a wire-format identifier). Must be O(prefix):
// inspect a bounded number of leading bytes, never a full parse.
//
// A false positive is recoverable — the claim still goes through the
// codec's Normalize and the registry's confidence threshold — but it
// costs a wasted parse and can steal traffic from the Tier-2 pattern
// probe, so probes should match protocol-distinctive markers, not
// merely plausible framing (a bare SSE "data:" prefix is NOT enough).
type Sniffer interface {
	LooksLike(raw []byte, meta Meta) bool
}
