package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/bodydecompress"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// debugBodyLimit caps how many bytes of the raw upstream body are captured
// and logged when slog DEBUG is enabled. Large enough to show a full SSE
// event from Gemini Flash (typically < 2 KiB) but small enough to avoid
// flooding the log with embedding / image responses.
const debugBodyLimit = 8192

// NewSpecAdapter wraps an [AdapterSpec] as an [Adapter]. Panics on a
// structurally invalid spec — a programming error caught at startup by
// [Registry.RegisterBuiltins].
//
// The adapter's forward-header allowlist defaults to the package's
// embedded resolved set ([forwardheader.Default]), which reproduces
// the historical hard-coded behavior. Production startup uses
// [NewSpecAdapterWithAllowlist] to inject the YAML-loaded allowlist
// instead.
func NewSpecAdapter(spec AdapterSpec, log *slog.Logger) Adapter {
	return NewSpecAdapterWithAllowlist(spec, nil, log)
}

// NewSpecAdapterWithAllowlist is [NewSpecAdapter] with an explicit
// resolved forward-header allowlist. Pass nil to fall back to
// [forwardheader.Default] (the embedded defaults). Used by
// cmd/ai-gateway/main.go (via provbuiltins.Register) to wire the
// operator-supplied YAML-resolved allowlist into every adapter at
// startup.
func NewSpecAdapterWithAllowlist(spec AdapterSpec, allowlist *forwardheader.Resolved, log *slog.Logger) Adapter {
	if !spec.Valid() {
		panic(fmt.Sprintf("providers: invalid AdapterSpec for format %q", spec.Format))
	}
	if log == nil {
		log = slog.Default()
	}
	return &specAdapter{spec: spec, allowlist: allowlist, log: log}
}

type specAdapter struct {
	spec      AdapterSpec
	allowlist *forwardheader.Resolved
	log       *slog.Logger
}

func (a *specAdapter) Format() Format { return a.spec.Format }

func (a *specAdapter) SupportsShape(shape typology.WireShape) bool {
	return a.spec.SupportsShape(shape)
}

// effectiveAllowlist returns the adapter's wired allowlist, or the
// package-level embedded default when a caller (typically a test) did
// not wire one. The returned pointer is process-stable; callers must
// not mutate it.
//
// Accept-Encoding is permanently excluded from the request-side
// allowlist. Forwarding it to the upstream disables Go
// net/http.Transport's transparent gzip auto-decompression, which
// caused a real Anthropic streaming SSE production incident — see
// https://pkg.go.dev/net/http#Transport. The hard denylist enforced
// at config load (forwardheader.Resolve) makes it impossible for an
// operator's YAML to re-add `accept-encoding`.
func (a *specAdapter) effectiveAllowlist() *forwardheader.Resolved {
	// Live atomic snapshot wins; the forwardHeaders allowlist is
	// resolved once from yaml at boot and never rewritten thereafter.
	// The construction-time pointer is retained only as a fallback for
	// tests that haven't seeded forwardheader.SetActive.
	if live := forwardheader.Active(); live != nil {
		return live
	}
	if a.allowlist != nil {
		return a.allowlist
	}
	return forwardheader.Default()
}

func (a *specAdapter) Execute(ctx context.Context, req Request) (*Response, error) {
	body, rewrites, urlOverride, err := a.prepareBodyFull(req)
	if err != nil {
		return nil, &ProviderError{
			Status:  http.StatusBadRequest,
			Code:    CodeInvalidRequest,
			Message: fmt.Sprintf("encode request: %v", err),
		}
	}
	return a.executeWithBodyAndURL(ctx, req, body, rewrites, urlOverride)
}

func (a *specAdapter) ExecuteWithBody(ctx context.Context, req Request, body []byte, rewrites []string) (*Response, error) {
	// Cache MISS / prepared-body fast path: PrepareBody discarded the
	// codec's URLOverride to keep its (body, rewrites, err) signature.
	// For endpoints where the action URL is selected by body shape
	// (Gemini embeddings — :embedContent for single, :batchEmbedContents
	// for batch), recover the override by peeking at the body. Without
	// this, a batch-shaped body lands on :embedContent and Gemini
	// returns HTTP 400 `Unknown name "requests": Cannot find field.`
	urlOverride := deriveURLOverrideFromBody(req.WireShape, a.spec.Format, body)
	return a.executeWithBodyAndURL(ctx, req, body, rewrites, urlOverride)
}

// deriveURLOverrideFromBody inspects an already-prepared wire body and
// returns the URLOverride suffix the codec would have emitted. Empty
// string means no override needed (transport default applies). Today
// only the Gemini embeddings endpoint needs this; extend the switch
// when other providers gain shape-driven URL action variants.
//
// Mirrors the dispatch the codec performs in
// packages/ai-gateway/internal/providers/specs/gemini/codec/embeddings.go
// — kept in sync with that codec's URLOverride literals.
func deriveURLOverrideFromBody(shape typology.WireShape, format Format, body []byte) string {
	if format != FormatGemini && format != FormatVertex {
		return ""
	}
	// Embeddings-kind shapes only. The prepared/failover paths may carry
	// either the caller's openai-embeddings shape OR — after cross-format
	// canonicalization — the target's native Gemini/Vertex embed shape. The
	// body-peek below is Gemini-wire-aware (requests[]/content), so all three
	// embeddings shapes route to the same single-vs-batch URL action recovery.
	switch shape {
	case typology.WireShapeOpenAIEmbeddings,
		typology.WireShapeGeminiEmbedContent,
		typology.WireShapeVertexEmbedContent:
	default:
		return ""
	}
	if len(body) == 0 {
		return ""
	}
	// gjson is already imported via the dispatch package's other files;
	// peek at top-level fields without allocating a full parse.
	if gjson.GetBytes(body, "requests").IsArray() {
		return ":batchEmbedContents"
	}
	if gjson.GetBytes(body, "content").Exists() {
		return ":embedContent"
	}
	return ""
}

// executeWithBodyAndURL is the internal implementation of ExecuteWithBody.
// urlOverride, when non-empty, replaces the Transport.BuildURL result.
// This enables codecs (e.g. Gemini embedding single vs batch) to select
// the correct URL path without changing the public Adapter interface.
func (a *specAdapter) executeWithBodyAndURL(ctx context.Context, req Request, body []byte, rewrites []string, urlOverride string) (*Response, error) {
	url, err := a.spec.Transport.BuildURL(req.Target, req.WireShape, req.Stream)
	if err != nil {
		return nil, &ProviderError{
			Status:  http.StatusInternalServerError,
			Code:    CodeInvalidRequest,
			Message: fmt.Sprintf("build url: %v", err),
		}
	}
	// Codec URLOverride takes precedence over the transport's default.
	// Used by Gemini embedding codec to switch between :embedContent and
	// :batchEmbedContents based on whether input is a single string or
	// an array of strings. The override replaces only the action suffix
	// in the URL — the transport-supplied base + model path stays intact.
	if urlOverride != "" {
		url = applyURLOverride(url, urlOverride)
	}

	method := http.MethodPost
	var reader io.Reader
	if req.WireShape == typology.WireShapeNone {
		method = http.MethodGet
	}
	if body != nil {
		reader = bytes.NewReader(body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, &ProviderError{
			Status:  http.StatusInternalServerError,
			Code:    CodeUpstreamError,
			Message: fmt.Sprintf("new request: %v", err),
		}
	}
	a.forwardHeaders(httpReq, req.Headers)
	if method != http.MethodGet && httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if err := a.spec.Transport.ApplyAuth(httpReq, req.Target); err != nil {
		return nil, &ProviderError{
			Status:  http.StatusUnauthorized,
			Code:    CodeAuthFailed,
			Message: fmt.Sprintf("apply auth: %v", err),
		}
	}

	if a.log.Enabled(ctx, slog.LevelDebug) && len(body) > 0 {
		preview := body
		if len(preview) > debugBodyLimit {
			preview = preview[:debugBodyLimit]
		}
		a.log.LogAttrs(ctx, slog.LevelDebug, "upstream request body",
			slog.String("format", string(a.spec.Format)),
			slog.String("url", url),
			slog.String("body", string(preview)),
		)
	}

	httpResp, err := a.spec.Transport.Do(ctx, httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &ProviderError{
				Status:  http.StatusGatewayTimeout,
				Code:    CodeTimeout,
				Message: fmt.Sprintf("upstream timeout: %v", err),
			}
		}
		return nil, &ProviderError{
			Status:  http.StatusBadGateway,
			Code:    CodeUpstreamError,
			Message: fmt.Sprintf("upstream: %v", err),
		}
	}

	if a.log.Enabled(ctx, slog.LevelDebug) {
		a.log.LogAttrs(ctx, slog.LevelDebug, "upstream response headers",
			slog.String("format", string(a.spec.Format)),
			slog.Int("status", httpResp.StatusCode),
			slog.Bool("stream", req.Stream),
			slog.String("content_type", httpResp.Header.Get("Content-Type")),
			slog.String("content_encoding", httpResp.Header.Get("Content-Encoding")),
			slog.String("content_disposition", httpResp.Header.Get("Content-Disposition")),
			slog.String("transfer_encoding", strings.Join(httpResp.TransferEncoding, ",")),
			slog.Int64("content_length", httpResp.ContentLength),
			slog.Bool("body_nil", httpResp.Body == nil),
		)
	}

	if httpResp.StatusCode >= 400 {
		defer httpResp.Body.Close() //nolint:errcheck
		// Error bodies are typically tiny; use the static ReadAllLimit
		// rather than the runtime cap so a misconfigured zero cap can
		// never starve the error message we surface to the caller.
		raw, _ := LimitedReadAll(httpResp.Body)
		// #77 — decompress non-gzip Content-Encoding (br / zstd / deflate)
		// that Go's transport leaves untouched, so the ErrorNormalizer's
		// JSON probe sees plain text. gzip is auto-decompressed by Go
		// (Accept-Encoding stripped → transport adds its own) and the
		// helper is a no-op via resp.Uncompressed=true.
		raw = bodydecompress.Decompress(raw, httpResp)
		pe := a.spec.ErrorNormalizer.Normalize(httpResp.StatusCode, httpResp.Header, raw)
		if pe == nil {
			pe = &ProviderError{
				Status:  httpResp.StatusCode,
				Code:    CodeUpstreamError,
				Message: fmt.Sprintf("upstream returned HTTP %d", httpResp.StatusCode),
				Raw:     raw,
			}
		}
		// Capture upstream headers so the handler can forward the
		// allowlisted subset (request-id, retry-after, …) even on the
		// error path. Clone is mandatory because the adapter is about to
		// drop the http.Response.
		pe.Headers = httpResp.Header.Clone()
		pe.TargetMethod = httpReq.Method
		pe.TargetPath = httpReq.URL.Path
		return nil, pe
	}

	if req.Stream {
		streamBody := httpResp.Body
		if a.log.Enabled(ctx, slog.LevelDebug) {
			streamBody = newDebugBody(streamBody, a.log, ctx, string(a.spec.Format))
		}
		session, err := a.spec.StreamDecoder.Open(streamBody, req.WireShape)
		if err != nil {
			_ = httpResp.Body.Close()
			return nil, &ProviderError{
				Status:  httpResp.StatusCode,
				Code:    CodeUpstreamError,
				Message: fmt.Sprintf("open stream: %v", err),
			}
		}
		return &Response{
			StatusCode:   httpResp.StatusCode,
			Headers:      httpResp.Header.Clone(),
			Stream:       session,
			BodyFormat:   a.spec.Format,
			Coerced:      rewrites,
			TargetMethod: httpReq.Method,
			TargetPath:   httpReq.URL.Path,
		}, nil
	}

	defer httpResp.Body.Close() //nolint:errcheck
	native, err := LimitedReadAllN(httpResp.Body, req.MaxResponseBytes)
	if err != nil {
		return nil, &ProviderError{
			Status:  http.StatusBadGateway,
			Code:    CodeUpstreamError,
			Message: fmt.Sprintf("read body: %v", err),
		}
	}
	// #77 — decompress non-gzip Content-Encoding (br / zstd / deflate)
	// upstream before SchemaCodec sees the bytes. A custom provider URL
	// fronted by Cloudflare / Akamai can legitimately respond in br even
	// when the gateway negotiated gzip; without this DecodeResponse
	// would fail with an opaque JSON parse error and rec.ResponseBody
	// would never be set. No-op for the gzip path Go's transport already
	// decompresses (resp.Uncompressed=true short-circuits the helper).
	native = bodydecompress.Decompress(native, httpResp)

	// Stamp resp_adapter_ms onto the request's PhaseSink so the handler's
	// finalize can merge it into latency_breakdown JSONB. No-op when no
	// sink is on ctx (e.g. probe / test paths).
	respAdapterStart := time.Now()
	decodeRes, err := a.spec.SchemaCodec.DecodeResponse(req.WireShape, native, httpResp.Header.Get("Content-Type"))
	if ps := traffic.PhaseSinkFromContext(ctx); ps != nil {
		ps.AddBreakdown(string(traffic.PhaseRespAdapter), int(time.Since(respAdapterStart).Milliseconds()))
	}
	if err != nil {
		return nil, &ProviderError{
			Status:  http.StatusBadGateway,
			Code:    CodeUpstreamError,
			Message: fmt.Sprintf("decode response: %v", err),
			Raw:     native,
		}
	}
	usage := decodeRes.Usage
	canonicalBody := decodeRes.CanonicalBody
	// Gemini embedding API never returns token counts in the response —
	// neither :embedContent nor :batchEmbedContents includes usage fields
	// (verified against raw upstream bodies). Without a usage figure the
	// gateway records prompt_tokens=0, which silently breaks per-call cost
	// accounting for Gemini embeddings. Recover by estimating from the wire
	// body's `text` payload (chars/4 heuristic) when Usage came back zeroed.
	// Both Response.Usage (internal) and the canonical body's usage block
	// (client-visible JSON) are updated so cost rollups and SDK numbers align.
	if req.WireShape == typology.WireShapeOpenAIEmbeddings &&
		(a.spec.Format == FormatGemini || a.spec.Format == FormatVertex) &&
		(usage.PromptTokens == nil || *usage.PromptTokens == 0) {
		var charCount int
		gjson.GetBytes(body, "content.parts").ForEach(func(_, p gjson.Result) bool {
			charCount += len(p.Get("text").String())
			return true
		})
		gjson.GetBytes(body, "requests").ForEach(func(_, r gjson.Result) bool {
			r.Get("content.parts").ForEach(func(_, p gjson.Result) bool {
				charCount += len(p.Get("text").String())
				return true
			})
			return true
		})
		if charCount > 0 {
			est := charCount / 4
			if est < 1 {
				est = 1
			}
			usage.PromptTokens = &est
			usage.TotalTokens = &est
			// Mirror into canonical body so the JSON the client receives
			// also carries the estimate. SetBytes is a no-op when the
			// canonical body is empty (early-failure paths).
			if len(canonicalBody) > 0 {
				if updated, sjErr := sjson.SetBytes(canonicalBody, "usage.prompt_tokens", est); sjErr == nil {
					canonicalBody = updated
				}
				if updated, sjErr := sjson.SetBytes(canonicalBody, "usage.total_tokens", est); sjErr == nil {
					canonicalBody = updated
				}
			}
		}
	}
	return &Response{
		StatusCode:   httpResp.StatusCode,
		Headers:      httpResp.Header.Clone(),
		Body:         canonicalBody,
		Usage:        usage,
		BodyFormat:   a.spec.Format,
		Coerced:      rewrites,
		TargetMethod: httpReq.Method,
		TargetPath:   httpReq.URL.Path,
	}, nil
}

func (a *specAdapter) Probe(ctx context.Context, target CallTarget) (*ProbeResult, error) {
	return a.spec.Transport.Probe(ctx, target)
}

// PrepareBody picks between passthrough and SchemaCodec.EncodeRequest.
// Returns the wire body, the list of in-place rewrites applied (empty when
// none), and any encoding error. Rewrites are only possible on the
// passthrough path; the codec path always returns an empty rewrite list.
// Idempotent; no side effects.
//
// Note: PrepareBody intentionally drops EncodeResult.URLOverride to keep
// the public Adapter interface stable. Use Execute (which calls
// prepareBodyFull internally) when the URLOverride must be honored.
func (a *specAdapter) PrepareBody(req Request) ([]byte, []string, error) {
	body, rewrites, _, err := a.prepareBodyFull(req)
	return body, rewrites, err
}

// prepareBodyFull is the internal variant of PrepareBody that also
// returns the EncodeResult.URLOverride. Called by Execute so that codecs
// that set URLOverride (e.g. Gemini embedding codec for batch vs single)
// actually influence the upstream URL.
func (a *specAdapter) prepareBodyFull(req Request) (body []byte, rewrites []string, urlOverride string, err error) {
	if req.WireShape == typology.WireShapeNone {
		return nil, nil, "", nil
	}
	// Use the passthrough rewrite path when both sides share the OpenAI wire
	// shape (e.g. FormatOpenAI → FormatMoonshot / FormatDeepSeek). The model
	// field must be rewritten even across distinct-but-compatible formats;
	// codec EncodeRequest on those adapters is an identity pass that would
	// leave the original model ID in the body.
	if req.BodyFormat == a.spec.Format || (req.BodyFormat.IsOpenAIFamily() && a.spec.Format.IsOpenAIFamily()) {
		b, rw, e := rewritePassthroughModel(req, a.spec.PassthroughRewrite)
		return b, rw, "", e
	}
	// Canonical OpenAI input needs codec translation. Codecs may apply
	// per-target rewrites of their own (e.g. spec_anthropic strips
	// temperature/top_p for claude-opus-4-7) and surface them so the
	// x-nexus-coerced header reflects what the upstream actually saw.
	result, encErr := a.spec.SchemaCodec.EncodeRequest(req.WireShape, req.Body, req.Target)
	if encErr != nil {
		return nil, nil, "", encErr
	}
	return result.Body, result.Rewrites, result.URLOverride, nil
}

// applyURLOverride replaces the action suffix of a provider URL with
// the given override. For Gemini this changes ":embedContent" →
// ":batchEmbedContents" (or vice versa) while leaving the base +
// model path intact. The override is expected to start with ":"
// (Gemini action suffix convention) or be a full URL replacement.
// If the override does not start with ":", the entire URL is replaced.
func applyURLOverride(baseURL, override string) string {
	if override == "" {
		return baseURL
	}
	if len(override) > 0 && override[0] == ':' {
		// Replace the last colon-action segment in the URL.
		if idx := strings.LastIndex(baseURL, ":"); idx >= 0 {
			return baseURL[:idx] + override
		}
		// No colon found — append the override.
		return baseURL + override
	}
	// Non-colon override: full URL replacement.
	return override
}

func rewritePassthroughModel(req Request, passthroughRewrite func(map[string]any, string) []string) ([]byte, []string, error) {
	// Strip the gateway-internal `nexus` namespace from the body before
	// any further work (PR #9). The passthrough path forwards req.Body
	// to upstream verbatim (modulo model rewrite), and the cross-format
	// codec path rebuilds the body from canonical fields — only this
	// passthrough is at risk of leaking gateway extensions to upstream.
	// Without this strip, OpenAI / Anthropic / Gemini / etc. reject the
	// request with "Unrecognized request argument supplied: nexus" (or
	// equivalent). canonicalext consumers (e.g. nexus.ext.<provider>.<key>)
	// only run on the cross-format codec path which never reaches here.
	body := stripNexusNamespace(req.Body)

	if req.Target.ProviderModelID == "" {
		return body, nil, nil
	}
	switch req.WireShape {
	case typology.WireShapeOpenAIChat, typology.WireShapeOpenAIEmbeddings, typology.WireShapeOpenAICompletionsLegacy:
	default:
		return body, nil, nil
	}
	if !req.BodyFormat.IsOpenAIFamily() {
		// Non-OpenAI-shape bodies (Anthropic Messages, Gemini generateContent,
		// Bedrock, Cohere, Replicate, ...) carry the model field in different
		// places or names; their per-format SchemaCodec.EncodeRequest is the
		// site that applies ct.ProviderModelID, not this passthrough path.
		return body, nil, nil
	}
	if len(body) == 0 {
		return body, nil, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, nil, err
	}
	payload["model"] = req.Target.ProviderModelID
	// Per-adapter rewrites are owned by the target adapter (Rule 3) and
	// reach us via the AdapterSpec.PassthroughRewrite callback. No
	// adapter-specific knowledge lives in this generic dispatch.
	var rewrites []string
	if passthroughRewrite != nil && req.WireShape == typology.WireShapeOpenAIChat {
		rewrites = passthroughRewrite(payload, req.Target.ProviderModelID)
	}
	if req.Stream && req.WireShape == typology.WireShapeOpenAIChat {
		applyStreamUsageOption(payload)
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	return out, rewrites, nil
}

// applyStreamUsageOption ensures stream_options.include_usage is true so
// OpenAI-compatible upstreams (OpenAI, Moonshot, Kimi, …) emit the final
// usage chunk in the SSE stream. Without this the openaiAccumulator cannot
// extract token counts and the audit row gets usage_extraction_status =
// streaming_unavailable instead of streaming_reported. Only touches the
// field when the caller has not already set it.
//
// Also defensively sets `stream: true` on the payload when missing. Native
// OpenAI-ingress streaming requests already carry `stream: true` from the
// client, so the rewrite is a no-op for the passthrough path. Cross-format
// ingresses (Gemini's :streamGenerateContent, Anthropic's stream:true)
// canonicalize to a chat-completions body that DOESN'T set the stream
// field — when the gateway then adds stream_options, OpenAI rejects with
// HTTP 400 "stream_options requires stream enabled". We're only inside
// this function because the request is streaming (gated by req.Stream
// upstream), so setting stream:true is always correct here.
func applyStreamUsageOption(payload map[string]any) {
	if _, ok := payload["stream"]; !ok {
		payload["stream"] = true
	}
	so, _ := payload["stream_options"].(map[string]any)
	if so == nil {
		so = map[string]any{}
		payload["stream_options"] = so
	}
	if _, ok := so["include_usage"]; !ok {
		so["include_usage"] = true
	}
}

// forwardHeaders copies allowlisted client headers from src onto the
// stripNexusNamespace drops the top-level `nexus` key from a JSON body
// using sjson's in-place delete. The `nexus` namespace is gateway-internal
// (canonicalext: ext.<provider>.<key>, ...) and must not reach any
// upstream provider — none of them understand it and most 4xx the
// request. Fast paths: bytes.Contains pre-check skips the sjson call for
// the common case where the client did not include any nexus extension.
// On any parse / delete error (malformed JSON, etc.) the original body is
// returned unchanged — the JSON parser downstream will surface the real
// error rather than silently dropping bytes.
func stripNexusNamespace(body []byte) []byte {
	if len(body) == 0 || !bytes.Contains(body, []byte(`"nexus"`)) {
		return body
	}
	out, err := sjson.DeleteBytes(body, "nexus")
	if err != nil {
		return body
	}
	return out
}

// outbound dst request. Anything not on the resolved request-side
// allowlist is dropped and counted against
// nexus_forward_header_dropped_total.
//
// The allowlist is precomputed per Format at config load
// (forwardheader.Resolve), so no per-request map allocation happens here.
func (a *specAdapter) forwardHeaders(dst *http.Request, src http.Header) {
	if len(src) == 0 {
		return
	}
	allowed := a.effectiveAllowlist().Request(string(a.spec.Format))
	adapterLabel := string(a.spec.Format)
	for k, vs := range src {
		lk := canonicalLower(k)
		if _, ok := allowed[lk]; !ok {
			emitForwardHeaderDrop("request", adapterLabel, forwardheader.BucketDroppedHeader(lk))
			continue
		}
		for _, v := range vs {
			dst.Header.Add(k, v)
		}
	}
}

// FilterResponseHeaders is a free function (not a method on Adapter)
// that returns a new http.Header containing only the upstream
// response headers permitted by the resolved response allowlist for
// the supplied Format. Per-request headers (e.g. x-request-id,
// rate-limit headers) are dropped on cache HIT — replaying a stale
// per-request value is worse than not surfacing it, since clients
// would correlate to a request that never happened.
//
// allowlist may be nil; callers get the embedded defaults via
// [forwardheader.Default]. format selects the per-adapter-type
// extension; passing an unknown Format returns just the base set.
//
// Headers not on either Static or PerRequest are dropped silently
// and counted against
// nexus_forward_header_dropped_total{direction="response"}.
//
// Kept as a free function so the handler does not need to type-assert
// on Adapter or pull the method through the interface (which would
// force every test mock of Adapter to grow it).
func FilterResponseHeaders(allowlist *forwardheader.Resolved, format Format, src http.Header, isCacheHit bool) http.Header {
	out := make(http.Header)
	if len(src) == 0 {
		return out
	}
	if allowlist == nil {
		allowlist = forwardheader.Default()
	}
	set := allowlist.Response(string(format))
	adapterLabel := string(format)
	for k, vs := range src {
		lk := canonicalLower(k)
		if _, ok := set.Static[lk]; ok {
			for _, v := range vs {
				out.Add(k, v)
			}
			continue
		}
		if _, ok := set.PerRequest[lk]; ok {
			if isCacheHit {
				// Strip on cache hit; replaying a stale per-request value
				// (request id, ratelimit-remaining, processing-ms) lies to
				// the client.
				continue
			}
			for _, v := range vs {
				out.Add(k, v)
			}
			continue
		}
		emitForwardHeaderDrop("response", adapterLabel, forwardheader.BucketDroppedHeader(lk))
	}
	return out
}

// debugBody wraps an io.ReadCloser to log every Read call at DEBUG level.
// The first debugBodyLimit bytes read are accumulated and emitted as a
// single "upstream stream body" record on Close, giving a clear snapshot
// of what the provider actually sent over the wire. Used only when slog
// DEBUG is enabled; never in production (gated by Enabled check).
type debugBody struct {
	inner  io.ReadCloser
	log    *slog.Logger
	ctx    context.Context //nolint:containedctx
	format string
	buf    bytes.Buffer
	capped bool
}

func newDebugBody(rc io.ReadCloser, log *slog.Logger, ctx context.Context, format string) *debugBody {
	return &debugBody{inner: rc, log: log, ctx: ctx, format: format}
}

func (d *debugBody) Read(p []byte) (int, error) {
	n, err := d.inner.Read(p)
	if n > 0 && !d.capped {
		remaining := debugBodyLimit - d.buf.Len()
		if remaining > 0 {
			take := n
			if take > remaining {
				take = remaining
				d.capped = true
			}
			d.buf.Write(p[:take])
		}
	}
	return n, err
}

func (d *debugBody) Close() error {
	if d.buf.Len() > 0 || d.capped {
		suffix := ""
		if d.capped {
			suffix = " (truncated)"
		}
		d.log.LogAttrs(d.ctx, slog.LevelDebug, "upstream stream body",
			slog.String("format", d.format),
			slog.Int("bytes_captured", d.buf.Len()),
			slog.Bool("capped", d.capped),
			slog.String("body", d.buf.String()+suffix),
		)
	} else {
		d.log.LogAttrs(d.ctx, slog.LevelDebug, "upstream stream body",
			slog.String("format", d.format),
			slog.Int("bytes_captured", 0),
			slog.String("body", "(empty — no bytes read from stream body)"),
		)
	}
	return d.inner.Close()
}

// canonicalLower returns the lower-cased canonical form of an HTTP
// header name. Reserved as a single chokepoint so future name
// normalization (e.g. tightening for non-ASCII inputs) lands here.
func canonicalLower(name string) string {
	// http.CanonicalMIMEHeaderKey already canonicalizes ASCII case;
	// lower-casing it once gives a stable key that matches the
	// pre-lowered allowlist. textproto would do the same, but the
	// stdlib already canonicalizes header keys on Header.Get/Set.
	b := make([]byte, len(name))
	for i := range len(name) {
		c := name[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}
