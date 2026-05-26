// Package payloadcapture provides an atomically swappable snapshot of the
// runtime payload-capture configuration shared by compliance-proxy,
// ai-gateway, and the agent. The admin editable knobs toggle
// request/response body storage for the audit pipeline (persisted into
// traffic_event_payload on the Hub side) and bound three independent
// byte caps:
//
//   - MaxRequestBytes     — inbound request body network read cap.
//   - MaxResponseBytes    — upstream non-streaming response network read cap.
//   - MaxInlineBodyBytes  — inline-vs-spill cutoff. Captured bodies at or
//     below this size travel as JSONB on traffic_event_payload.inline_*_body;
//     larger bodies are written to the SpillStore backend by the producer
//     and the row keeps a *_spill_ref. Never bounds the bytes forwarded to
//     the upstream provider or the client.
//
// The authoritative copy of these values lives in
// system_metadata["payload_capture.config"] on the platform database.
// Data-plane services load it at startup and refresh it from the same row
// on each `payload_capture` shadow invalidation delivered by Nexus Hub.
package payloadcapture

// DefaultMaxInlineBodyBytes is the inline-vs-spill cutoff used when the
// admin has not set an explicit value. 256 KiB matches Postgres' efficient
// JSONB inline range. Bodies at or below this size are stored inline on
// traffic_event_payload.inline_*_body; larger bodies are spilled to the
// configured SpillStore backend by the producer.
const DefaultMaxInlineBodyBytes int64 = 256 * 1024

// DefaultMaxRequestBytes is the default network read cap for inbound
// request bodies on compliance-proxy and ai-gateway proxy handlers. The
// 10 MiB ceiling matches the historical hard-coded const that the runtime
// store replaces and comfortably covers tool-heavy LLM clients (system
// prompt + tool schemas) without inviting unbounded uploads. Overflow on
// this cap surfaces as `413 PAYLOAD_TOO_LARGE` to the caller.
const DefaultMaxRequestBytes int64 = 10 * 1024 * 1024 // 10 MiB

// DefaultMaxResponseBytes is the default network read cap for upstream
// non-streaming responses pulled by provider adapters via
// providers.LimitedReadAll. Streaming responses are governed by the
// per-stream policies in shared/streaming and are not affected by this
// cap. Overflow surfaces as `502 upstream_error` to the caller.
const DefaultMaxResponseBytes int64 = 10 * 1024 * 1024 // 10 MiB

// Config is an immutable value holding the runtime knobs that govern
// payload capture. All callers must treat the value as read-only — updates
// happen by swapping the pointer inside Store, not by mutating an existing
// Config. Instances are copied by value on every Store.Get.
type Config struct {
	// StoreRequestBody enables copying the client-original request body
	// into the audit pipeline. When false, the request body bytes are
	// still read for hook inspection and upstream forwarding but are not
	// persisted downstream.
	StoreRequestBody bool

	// StoreResponseBody enables copying the upstream response body into
	// the audit pipeline. Streaming (text/event-stream) responses are
	// captured via an end-of-stream tee and emitted through the same
	// EmitBody path as non-streaming responses.
	StoreResponseBody bool

	// MaxInlineBodyBytes is the inline-vs-spill cutoff for the captured
	// copy that hits the audit pipeline. Bodies at or below this size
	// travel as JSONB on traffic_event_payload.inline_*_body; larger
	// bodies are written to the SpillStore backend by the producer and
	// the row keeps a *_spill_ref. Values <= 0 are coerced to
	// DefaultMaxInlineBodyBytes by DecodeConfigJSON. This cap NEVER
	// bounds the bytes forwarded to the upstream provider or the client.
	MaxInlineBodyBytes int64

	// MaxRequestBytes is the network read cap on the inbound request
	// body. The proxy handler reads up to this many bytes from r.Body;
	// requests larger than this are rejected with 413 and never reach
	// the upstream provider. Values <= 0 are treated as
	// "use DefaultMaxRequestBytes" by consumers.
	MaxRequestBytes int64

	// MaxResponseBytes is the network read cap on the upstream
	// non-streaming response body. Provider adapters wrap their body
	// reads in io.LimitReader at this cap; overflow surfaces as a 502.
	// Values <= 0 are treated as "use DefaultMaxResponseBytes" by
	// consumers.
	MaxResponseBytes int64
}

// DefaultConfig returns the conservative zero-risk configuration: both
// capture flags disabled (no body is persisted until an admin enables
// them), MaxInlineBodyBytes set to DefaultMaxInlineBodyBytes (256 KiB —
// matching Postgres' efficient JSONB inline range), and a generous 10
// MiB network read cap on each of the inbound request and upstream
// response. Services that fail to locate
// system_metadata["payload_capture.config"] at startup initialise
// their Store with this value.
func DefaultConfig() Config {
	return Config{
		StoreRequestBody:   false,
		StoreResponseBody:  false,
		MaxInlineBodyBytes: DefaultMaxInlineBodyBytes,
		MaxRequestBytes:    DefaultMaxRequestBytes,
		MaxResponseBytes:   DefaultMaxResponseBytes,
	}
}
