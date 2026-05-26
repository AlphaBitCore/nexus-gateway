package core

import (
	"context"
	"io"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// AdapterSpec is the declarative unit that each provider subpackage
// returns. The generic [specAdapter] composes these four components
// into an [Adapter] implementation.
//
// Forward-header policy is no longer carried on AdapterSpec: it is
// owned by the runtime [forwardheader.Resolved] structure built from
// YAML at startup. Adding a per-adapter-type extension is a YAML edit,
// not a code change.
type AdapterSpec struct {
	Format          Format
	Transport       Transport
	SchemaCodec     SchemaCodec
	StreamDecoder   StreamDecoder
	ErrorNormalizer ErrorNormalizer
	// PassthroughRewrite is the per-adapter rewrite hook applied on the
	// passthrough path (BodyFormat already matches Format, no codec
	// translation runs). It mutates the OpenAI-shape payload in place
	// and returns the list of rewrites applied (formatted as
	// "<from>→<to>") so the handler can stamp x-nexus-coerced.
	// Used for per-model wire quirks owned by the adapter (e.g.
	// spec_openai strips temperature/top_p and renames max_tokens for
	// gpt-5 / o-series reasoning models; spec_moonshot strips
	// caller-supplied temperature for kimi-k2.5 / k2.6 fixed-temp
	// models). Nil means no passthrough rewrites — the body goes
	// through unchanged.
	//
	// Per provider-adapter-architecture.md §3a Rule 3, per-model wire
	// quirks belong to the adapter that talks to that wire, not to the
	// generic spec_adapter dispatcher.
	PassthroughRewrite func(payload map[string]any, modelID string) []string

	// RequestShapes lists the typology.WireShape values this adapter
	// natively serves at the codec boundary (i.e. the WireShape values
	// the codec's EncodeRequest/DecodeResponse will accept without
	// returning an "unsupported shape" error).
	//
	// Values come from packages/shared/transport/typology — e.g.
	// WireShapeOpenAIChat, WireShapeAnthropicMessages,
	// WireShapeGeminiGenerateContent, WireShapeBedrockConverse,
	// WireShapeBedrockEmbeddings, WireShapeCohereChat,
	// WireShapeCohereEmbed, WireShapeVoyageEmbeddings, etc.
	//
	// Empty / nil means the adapter only natively serves
	// typology.WireShapeOpenAIChat. New adapter authors should declare
	// RequestShapes explicitly even when there is only one entry; an
	// explicit declaration is easier to audit.
	//
	// Per provider-adapter-architecture.md §3a Rule 7 (binding), adding
	// a new WireShape to an adapter requires empirical evidence (a
	// captured 200 from the real upstream endpoint for that shape).
	// Speculative declarations route real traffic to a 404/400 surface.
	RequestShapes []typology.WireShape
}

// SupportsShape reports whether this adapter natively serves the given
// WireShape. Empty RequestShapes defaults to [WireShapeOpenAIChat] so
// adapters that have not declared shapes keep the historical "chat-only"
// behaviour.
func (s AdapterSpec) SupportsShape(shape typology.WireShape) bool {
	if len(s.RequestShapes) == 0 {
		return shape == typology.WireShapeOpenAIChat
	}
	for _, sh := range s.RequestShapes {
		if sh == shape {
			return true
		}
	}
	return false
}

// Valid performs a structural check on a spec without exercising it.
func (s AdapterSpec) Valid() bool {
	return s.Format.Valid() &&
		s.Transport != nil &&
		s.SchemaCodec != nil &&
		s.StreamDecoder != nil &&
		s.ErrorNormalizer != nil
}

// Transport owns URL construction, authentication, HTTP client
// handling, and Probe. Implementations typically embed a shared
// *http.Client tuned for the provider's SLA characteristics.
type Transport interface {
	// BuildURL composes BaseURL + wire-shape-specific path + any
	// provider-specific URL segments (Azure deployment path, Gemini
	// model-in-path, etc.). Callers never concatenate paths outside
	// the Transport implementation. The shape parameter selects which
	// wire path (chat / responses / embeddings / etc.) the URL targets.
	BuildURL(target CallTarget, shape typology.WireShape, stream bool) (string, error)

	// ApplyAuth sets authentication headers (Authorization, x-api-key,
	// api-key, x-goog-api-key, or SigV4/OAuth signing) on the outbound
	// request. Implementations may read from target.Extras.
	ApplyAuth(r *http.Request, target CallTarget) error

	// Do executes the prepared request and returns the raw http.Response.
	// Implementations may wrap a shared *http.Client with provider-tuned
	// timeouts and transport options.
	Do(ctx context.Context, r *http.Request) (*http.Response, error)

	// Probe performs the adapter-specific health check. It is invoked
	// by the registry's health pipeline and by the admin "test provider"
	// API.
	Probe(ctx context.Context, target CallTarget) (*ProbeResult, error)
}

// EncodeResult is the structured output of [SchemaCodec.EncodeRequest].
// Body is the provider-native request bytes. ContentType is the value to
// use for the Content-Type header (typically "application/json"). Headers
// carries any extra provider-specific request headers the codec wants to
// inject (e.g. x-cohere-version). URLOverride replaces the Transport's
// BuildURL result when non-empty (used by providers that embed the endpoint
// in the URL path). Rewrites lists any in-place request mutations applied
// (formatted "<from>→<to>") for x-nexus-coerced stamping.
//
// Artifact/job fields are not needed here — codecs produce only wire bytes on
// the request side.
type EncodeResult struct {
	Body        []byte
	ContentType string
	Headers     http.Header
	URLOverride string
	Rewrites    []string
}

// DecodeResult is the structured output of [SchemaCodec.DecodeResponse].
// CanonicalBody is the OpenAI chat-completions shaped response bytes.
// Usage is the extracted token-accounting envelope. Artifacts carries any
// media outputs (images, audio, video) produced by image/audio/video codecs;
// chat and embedding codecs leave it nil.
type DecodeResult struct {
	CanonicalBody []byte
	Usage         Usage
	Artifacts     []ArtifactRef
}

// SchemaCodec converts between a provider's native wire schema and the
// canonical OpenAI shape. [specAdapter] skips the codec when the
// inbound request is already in the adapter's native format
// (passthrough fast path).
//
// EncodeRequest returns [EncodeResult] (Body, ContentType, Headers,
// URLOverride, Rewrites). DecodeResponse returns [DecodeResult] (CanonicalBody,
// Usage, Artifacts) and accepts a contentType parameter so codecs that
// need to disambiguate the response MIME type can do so without parsing
// Content-Type themselves. All chat codecs pass _ = contentType; pass ""
// from callers that don't know the type.
type SchemaCodec interface {
	// EncodeRequest converts a canonical OpenAI-shaped body into this
	// provider's native request body. If canonicalBody == nil the caller
	// already has native bytes (passthrough); implementations should
	// return a zero EncodeResult with nil error in that case. Rewrites on
	// the result lists any in-place mutations applied (formatted as
	// "<from>→<to>" or "<param>→removed") so the handler can stamp them
	// onto the x-nexus-coerced response header. The shape parameter selects
	// which wire-shape the codec produces (chat / responses / embeddings /
	// etc.) — see typology.WireShape.
	EncodeRequest(shape typology.WireShape, canonicalBody []byte, target CallTarget) (EncodeResult, error)

	// DecodeResponse converts a native response body into canonical
	// OpenAI shape AND extracts Usage. Both CanonicalBody and Usage on
	// the result must be populated when possible. contentType is the
	// upstream Content-Type header value; pass "" when unknown. Codecs
	// that only serve application/json may ignore it (_ = contentType).
	// The shape parameter discriminates the wire-shape the codec must
	// decode from (chat / responses / embeddings / etc.).
	DecodeResponse(shape typology.WireShape, nativeBody []byte, contentType string) (DecodeResult, error)
}

// StreamDecoder wraps the provider's streaming response body and
// produces a uniform [StreamSession]. The decoder owns reading from r
// and must call r.Close() when the stream terminates. The shape
// parameter selects the wire-shape grammar of the stream (e.g.
// openai-chat SSE delta vs openai-responses event stream).
type StreamDecoder interface {
	Open(r io.ReadCloser, shape typology.WireShape) (StreamSession, error)
}

// ErrorNormalizer converts a provider error response (status, headers,
// body) into a canonical [ProviderError]. Implementations should map
// the provider's own error type into one of the canonical codes
// declared as `Code*` constants in types.go.
type ErrorNormalizer interface {
	Normalize(status int, headers http.Header, body []byte) *ProviderError
}
