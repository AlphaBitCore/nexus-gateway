// Package extract reduces provider-specific SSE / chunked-NDJSON byte
// streams to a canonical `{prompt, completion}` text view. The hook
// pipeline only ever sees this canonical view, never raw provider frames,
// so a single regex / classifier hook works against every provider without
// per-provider conditionals.
//
// One ContentExtractor implementation per provider lives in this package
// (openai-api, anthropic-messages, google-gemini, chatgpt-web). The
// Registry maps a provider id (matching `Provider.adapter_type` for
// ai-gateway and `interception_domain.adapter_id` for compliance-proxy /
// agent) to its extractor.
//
// The interface is split into a request-side one-shot extraction and a
// response-side incremental accumulator so the streaming pipeline can
// invoke them at different lifecycle points without re-implementing
// per-provider parsing twice.
package extract

import (
	"sync"
)

// ExtractedContent is the canonical text view of a single direction
// (request or response). `Truncated` reflects whether the extractor
// hit a per-stream cap and dropped tail content.
type ExtractedContent struct {
	Prompt     string
	Completion string
	Truncated  bool
}

// ExtractedDelta is one increment produced by a per-frame parse on the
// response side. `Prompt` is non-empty only when the provider echoes the
// caller's prompt inside the stream (chatgpt-web does this in the
// `input_message` frame). `Completion` is the model-output delta. Empty
// strings on both fields mean the frame carried no content (e.g. a
// metadata-only event); the accumulator drops these silently.
type ExtractedDelta struct {
	Prompt     string
	Completion string
}

// ContentExtractor is the per-provider parser interface.
type ContentExtractor interface {
	// ID returns the canonical extractor name (matches the adapter id
	// used in `interception_domain.adapter_id` / `Provider.adapter_type`).
	ID() string

	// ExtractRequest parses a single buffered request body and returns
	// the prompt text plus any response-side echo content the caller
	// already supplied (rare; chatgpt-web echoes the user message
	// inline in the response stream rather than inside the request).
	ExtractRequest(body []byte) ExtractedContent

	// NewAccumulator returns a fresh per-stream accumulator for the
	// response side. Each accumulator is single-flight per stream.
	NewAccumulator() Accumulator
}

// Accumulator parses incremental SSE frames or chunked-NDJSON frames and
// builds the canonical content view as the stream progresses. Implementations
// are NOT goroutine-safe — the caller (compliance-proxy / ai-gateway / agent
// streaming pipeline) owns one accumulator per stream and feeds frames in
// arrival order from a single goroutine.
type Accumulator interface {
	// Feed parses one already-framed event payload (the bytes after
	// `data: ` and before the trailing blank line for SSE; or one NDJSON
	// row for Gemini-style chunked formats). Returns the increment
	// extracted from this frame; safe to ignore an empty delta.
	Feed(frame []byte) ExtractedDelta

	// Snapshot returns the full assembled content so far without
	// resetting the accumulator. Safe to call mid-stream (for
	// chunked_async checkpoint hooks) and at stream end.
	Snapshot() ExtractedContent

	// Truncate marks the accumulator as truncated; subsequent Snapshot
	// returns Truncated=true. Used by the streaming pipeline when the
	// per-stream max-buffer cap is hit.
	Truncate()
}

// Registry maps an extractor id to its constructor. Read-only at runtime;
// `RegisterBuiltins` populates it once at process start. A lookup for an
// unknown id returns the noop fallback so non-LLM domains still have an
// extractor to plug into the streaming pipeline.
type Registry struct {
	mu sync.RWMutex
	m  map[string]ContentExtractor
}

func NewRegistry() *Registry {
	return &Registry{m: make(map[string]ContentExtractor)}
}

// Register installs an extractor under its ID. Panics on duplicate
// registration so production builds catch the configuration error at
// process start rather than at request time.
func (r *Registry) Register(e ContentExtractor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.m[e.ID()]; exists {
		panic("extract.Registry: duplicate extractor id: " + e.ID())
	}
	r.m[e.ID()] = e
}

// Get returns the extractor for `id`, falling back to the noop extractor
// when no provider-specific implementation is registered.
func (r *Registry) Get(id string) ContentExtractor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.m[id]; ok {
		return e
	}
	return noopExtractor{}
}

// RegisterBuiltins installs the four production extractors (openai-api,
// anthropic-messages, google-gemini, chatgpt-web). Tests construct a
// Registry without builtins to inject fakes.
func RegisterBuiltins(r *Registry) {
	r.Register(NewOpenAIAPIExtractor())
	r.Register(NewChatGPTWebExtractor())
}
