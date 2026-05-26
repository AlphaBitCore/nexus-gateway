// Package policy carries the cross-service `Policy` value that governs how
// each data plane handles streaming compliance. The same struct is consumed
// by ai-gateway, compliance-proxy, and agent — each service has its own
// resolver (`ResolvePolicyByHost` for compliance-proxy/agent against
// interception_domain; `ResolvePolicyByProviderID` for ai-gateway against
// the Provider table) but the merged result is a single Policy instance
// fed into shared/streaming.
package policy

// Mode discriminates the three streaming compliance behaviours.
//
//   - PassThrough         — relay only, no hook, no body capture.
//   - BufferFullBlock     — buffer entire response then run response-stage
//     hook on the full extracted content; `fail_close`
//     can HTTP-451 the client (response is held until
//     the hook decides).
//   - ChunkedAsync        — relay bytes in real time AND accumulate via the
//     per-provider ContentExtractor; the hook runs on
//     every `ChunkBytes` of extracted content plus
//     once at stream end. Cannot reject (audit-only).
type Mode string

const (
	ModePassThrough     Mode = "passthrough"
	ModeBufferFullBlock Mode = "buffer_full_block"
	ModeChunkedAsync    Mode = "chunked_async"
)

// FailBehavior selects the action when a hook errors / times out / runs into
// an oversized buffer. See `docs/users/product/architecture.md` §"SSE Compliance
// Pipeline" for the full table.
type FailBehavior string

const (
	FailOpen  FailBehavior = "fail_open"
	FailClose FailBehavior = "fail_close"
)

// Policy is the resolved per-stream config. NULL/zero values in any column
// of the per-resource override are filled from the global default by the
// resolver before this struct reaches the streaming pipeline.
//
// Invariant: Policy MUST hold only value-type fields (scalars + named
// string types). Store.Get() returns Policy by value to give callers
// a goroutine-safe snapshot; adding a slice, map, or pointer field
// here would share the backing memory with the live snapshot and
// callers could mutate the next reader's view. If a future field
// genuinely needs a reference type, change Store.Get() to deep-copy
// at the same time — do not relax this invariant silently.
type Policy struct {
	Mode                Mode
	ChunkBytes          int          // chunked_async only
	HookTimeoutMs       int          // per-hook execution budget
	MaxBufferBytes      int          // per-stream in-memory cap
	FailBehavior        FailBehavior // applied on hook error/timeout/oversize
	CaptureRequestBody  bool
	CaptureResponseBody bool
	RawSpillEnabled     bool // when true, oversize bodies fall back to SpillStore
}

// IsValid reports whether the policy fields fall in acceptable ranges.
// Resolvers call this after merging override + global to surface
// configuration errors at admin write time rather than at request time.
func (p Policy) IsValid() bool {
	switch p.Mode {
	case ModePassThrough, ModeBufferFullBlock, ModeChunkedAsync:
	default:
		return false
	}
	switch p.FailBehavior {
	case FailOpen, FailClose:
	default:
		return false
	}
	if p.ChunkBytes < 0 || p.HookTimeoutMs < 0 || p.MaxBufferBytes < 0 {
		return false
	}
	return true
}

// DefaultPolicy returns the conservative baseline used when
// system_metadata['streaming_compliance.config'] has not been written yet.
// Bias toward "audit-only, real-time UX" so a fresh deployment never
// silently breaks SSE clients.
func DefaultPolicy() Policy {
	return Policy{
		Mode:                ModePassThrough,
		ChunkBytes:          8 * 1024,
		HookTimeoutMs:       2000,
		MaxBufferBytes:      64 * 1024 * 1024,
		FailBehavior:        FailOpen,
		CaptureRequestBody:  false,
		CaptureResponseBody: false,
		RawSpillEnabled:     false,
	}
}
