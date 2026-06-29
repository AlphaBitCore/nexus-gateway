package dispatch

import (
	"bytes"
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/bodydecompress"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

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

func (a *specAdapter) ExecuteWithBody(ctx context.Context, req Request, body []byte, rewrites []string, urlOverride string) (*Response, error) {
	// Cache MISS / prepared-body fast path: the codec's URLOverride is
	// threaded through PrepareBody → the cache layer → here, so a
	// shape-driven action URL (Gemini :embedContent vs :batchEmbedContents)
	// reaches the dispatched URL without the generic dispatcher re-deriving
	// it by peeking at provider-specific body fields.
	return a.executeWithBodyAndURL(ctx, req, body, rewrites, urlOverride)
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

	// Non-streaming upstream calls get an explicit per-request deadline from
	// the live ActiveConfig().Timeout budget. The upstream http.Client.Timeout
	// is intentionally 0 (specutil), so without this the only non-stream bound
	// was an unrelated server write-timeout that never cancelled the upstream
	// goroutine — the operator-tuned upstream.timeout was dead on the hot path.
	// context.WithTimeout only ever tightens: if the caller
	// already set an earlier deadline, that one still wins. Streaming responses
	// are NOT wrapped: the stream body is read lazily by the caller after this
	// function returns, so a deadline anchored to this stack frame would abort
	// a healthy long-lived stream; their time-to-headers is bounded by the
	// Transport's ResponseHeaderTimeout instead.
	callCtx := ctx
	if !req.Stream {
		if budget := specutil.ActiveConfig().Timeout; budget > 0 {
			var cancel context.CancelFunc
			callCtx, cancel = context.WithTimeout(ctx, budget)
			defer cancel()
		}
	}

	method := http.MethodPost
	var reader io.Reader
	if req.WireShape == typology.WireShapeNone {
		method = http.MethodGet
	}
	if body != nil {
		reader = bytes.NewReader(body)
	}

	httpReq, err := http.NewRequestWithContext(callCtx, method, url, reader)
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

	httpResp, err := a.spec.Transport.Do(callCtx, httpReq, req.Target)
	if err != nil {
		if callCtx.Err() != nil {
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
		// Decompress non-gzip Content-Encoding (br / zstd / deflate)
		// that Go's transport leaves untouched, so the ErrorNormalizer's
		// JSON probe sees plain text. gzip is auto-decompressed by Go
		// (Accept-Encoding stripped → transport adds its own) and the
		// helper is a no-op via resp.Uncompressed=true. Error bodies are
		// tiny; the default decompressed-size bound (50 MiB) suffices and a
		// truncation here only affects the diagnostic error message.
		raw, _ = bodydecompress.Decompress(raw, httpResp, 0)
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
	native, readTruncated, err := LimitedReadAllN(httpResp.Body, req.MaxResponseBytes)
	if err != nil {
		return nil, &ProviderError{
			Status:  http.StatusBadGateway,
			Code:    CodeUpstreamError,
			Message: fmt.Sprintf("read body: %v", err),
		}
	}
	if readTruncated {
		// The upstream non-streaming body exceeded the read cap and
		// was clamped. The usage block (typically at the JSON tail) may be
		// missing or partial, so the eventual token counts cannot be trusted;
		// the handler stamps usage_extraction_status="truncated" off
		// Response.Truncated below.
		a.log.Warn("upstream response exceeded read cap; usage extraction may be incomplete",
			slog.Int64("max_response_bytes", req.MaxResponseBytes),
			slog.String("format", string(a.spec.Format)),
		)
	}
	// Decompress non-gzip Content-Encoding (br / zstd / deflate)
	// upstream before SchemaCodec sees the bytes. A custom provider URL
	// fronted by Cloudflare / Akamai can legitimately respond in br even
	// when the gateway negotiated gzip; without this DecodeResponse
	// would fail with an opaque JSON parse error and rec.ResponseBody
	// would never be set. No-op for the gzip path Go's transport already
	// decompresses (resp.Uncompressed=true short-circuits the helper).
	// The compressed read above is bounded by req.MaxResponseBytes; the
	// decompressed expansion is bounded by bodydecompress's own cap so a
	// br/zstd decompression bomb cannot OOM the gateway.
	var decompTruncated bool
	native, decompTruncated = bodydecompress.Decompress(native, httpResp, 0)
	if decompTruncated {
		a.log.Warn("upstream response exceeded decompressed-size bound; treating as opaque",
			slog.String("content_encoding", httpResp.Header.Get("Content-Encoding")),
			slog.String("format", string(a.spec.Format)),
		)
	}

	// Stamp resp_adapter_ms onto the request's PhaseSink so the handler's
	// finalize can merge it into latency_breakdown JSONB. No-op when no
	// sink is on ctx (e.g. probe / test paths).
	respAdapterStart := time.Now()
	decodeRes, err := a.spec.SchemaCodec.DecodeResponse(req.WireShape, native, httpResp.Header.Get("Content-Type"), DecodeContext{Target: req.Target, RequestBody: body})
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
	// The Gemini/Vertex embedding wire never returns token counts; the
	// chars/4 prompt-token estimate that recovers them now lives in the
	// Gemini embedding codec (decodeGeminiEmbeddingResponse), which
	// receives the wire request body via DecodeContext — no provider-name
	// branch in this generic dispatcher.
	//
	// Generic embeddings model back-fill: the canonical embeddings response
	// must echo the requested model so OpenAI SDK callers can read it. Most
	// decoders stamp the model from the wire response, but providers whose
	// embedding wire shape carries no model field (Gemini / Vertex) leave it
	// empty because the stateless SchemaCodec.DecodeResponse interface does
	// not receive the CallTarget. Back-fill from the resolved ProviderModelID
	// here — this is a format-agnostic rule (no provider-name branch) and is a
	// no-op when the decoder already stamped a non-empty model.
	if typology.KindFromWireShape(req.WireShape) == typology.EndpointKindEmbeddings &&
		len(canonicalBody) > 0 && req.Target.ProviderModelID != "" {
		if m := gjson.GetBytes(canonicalBody, "model"); !m.Exists() || m.Str == "" {
			if updated, sjErr := sjson.SetBytes(canonicalBody, "model", req.Target.ProviderModelID); sjErr == nil {
				canonicalBody = updated
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
		// Either the raw read cap (readTruncated) or the
		// decompressed-size bound (decompTruncated) clamped the bytes fed to
		// DecodeResponse, so the parsed usage is incomplete. Surface it so the
		// handler refuses to report usage_extraction_status="ok".
		Truncated: readTruncated || decompTruncated,
	}, nil
}

func (a *specAdapter) Probe(ctx context.Context, target CallTarget) (*ProbeResult, error) {
	return a.spec.Transport.Probe(ctx, target)
}

// transportModelLister is a capability interface optionally implemented by
// OpenAI-compatible transports. Only transports that expose a /v1/models
// list endpoint implement this; non-OpenAI transports do not, which is the
// correct signal that model discovery is unsupported for that adapter.
type transportModelLister interface {
	ListModels(ctx context.Context, target CallTarget) ([]string, error)
}

// ListModels delegates to the underlying transport when it implements the
// optional [transportModelLister] capability (OpenAI and OpenAI-compatible
// transports), and returns (nil, false) otherwise. The handler uses the
// boolean to distinguish "discovery supported" from "adapter does not support
// discovery" without branching on format names.
func (a *specAdapter) ListModels(ctx context.Context, target CallTarget) ([]string, bool, error) {
	lister, ok := a.spec.Transport.(transportModelLister)
	if !ok {
		return nil, false, nil
	}
	ids, err := lister.ListModels(ctx, target)
	return ids, true, err
}

// PrepareBody picks between passthrough and SchemaCodec.EncodeRequest.
// Returns the wire body, the list of in-place rewrites applied (empty when
// none), and any encoding error. Rewrites are only possible on the
// passthrough path; the codec path always returns an empty rewrite list.
// Idempotent; no side effects.
//
// PrepareBody returns the codec's URLOverride alongside the body so a
// caller reusing the prepared body on the cache-MISS fast path can pass
// it into ExecuteWithBody — the override (e.g. Gemini :batchEmbedContents)
// then reaches the dispatched URL instead of being re-derived from the
// body in generic dispatch.
func (a *specAdapter) PrepareBody(req Request) ([]byte, []string, string, error) {
	return a.prepareBodyFull(req)
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
		b, rw, e := rewritePassthroughModel(req, a.spec.PassthroughRewrite, a.spec.PassthroughRewriteApplies)
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

func rewritePassthroughModel(req Request, passthroughRewrite func(map[string]any, string) []string, rewriteApplies func(string) bool) ([]byte, []string, error) {
	// Strip the gateway-internal `nexus` namespace from the body before
	// any further work The passthrough path forwards req.Body
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
	// Fast path: the map[string]any unmarshal+marshal below is a full round-trip
	// of the (often large) body whose ONLY purpose, in the common case, is to set
	// the top-level `model`. That round-trip is needed only when a per-adapter
	// rewrite or the streaming-usage option must mutate the parsed map — i.e.
	// WireShapeOpenAIChat with a non-nil passthroughRewrite or a streaming
	// request. Otherwise a surgical sjson edit of just `model` avoids decoding the
	// whole body (the dominant per-request allocation on the hot path).
	//
	// Guard: more than one `"model"` occurrence may be a duplicate top-level key.
	// sjson edits the FIRST occurrence; JSON parsers and the cache-key
	// canonicaliser take last-wins — so fall back to the map path (last-wins) for
	// correctness on that pathological shape. The prepared body's byte layout is
	// not persisted (traffic_event stores the original client body) and the cache
	// key re-canonicalises, so sjson's layout difference vs the map path is safe.
	// The map round-trip is needed only when something must mutate the parsed
	// body: a streaming-usage option, or a per-adapter rewrite that ACTUALLY
	// applies to this model. PassthroughRewriteApplies answers the latter without
	// decoding — so a non-reasoning model on an adapter whose only rewrite is the
	// reasoning-quirk strip takes the surgical sjson path instead of the
	// (dominant) full decode+marshal. Nil probe ⇒ assume it may apply (keep the
	// map path), preserving prior behavior.
	rewriteMayApply := passthroughRewrite != nil &&
		(rewriteApplies == nil || rewriteApplies(req.Target.ProviderModelID))
	needsMap := req.WireShape == typology.WireShapeOpenAIChat &&
		(req.Stream || rewriteMayApply)
	// Passthrough does at most ONE thing to the body: set the top-level `model`.
	// We deliberately do NOT pre-validate the whole body with json.Valid first.
	// A malformed client body is the client's problem — sjson forwards it (or
	// errors) and the upstream returns the error; an enterprise caller does not
	// submit garbage JSON, and even under attack an upstream 400 is harmless to
	// the gateway. The full-body gjson.ValidBytes scan it replaced was the single
	// largest CPU cost on the passthrough hot path (~46% of this function) for a
	// validation the upstream already performs. The dup-key guard stays: sjson
	// edits the FIRST `"model"`, but JSON parsers take last-wins, so a body with
	// two top-level model keys must fall to the map path or the routing rewrite
	// would silently target the wrong model — a correctness bug, not a client
	// error.
	if !needsMap && bytes.Count(body, []byte(`"model"`)) <= 1 {
		// Only rewrite when the body is a JSON object. sjson.SetBytes on a
		// non-object body (malformed garbage, empty, a bare scalar/array) does
		// NOT forward the client's bytes — it FABRICATES a synthetic
		// {"model":X}. We would then send that invented request upstream on the
		// shared credential while the audit row stored the client's original
		// bytes (wire ≠ audit). cd3164876's intent was to FORWARD the malformed
		// body and let the upstream return the error; honour that by forwarding
		// non-object bodies unchanged. The check is a leading-byte scan (no full
		// parse), so the json.Valid-removal perf win is preserved.
		if t := bytes.TrimLeft(body, " \t\r\n"); len(t) == 0 || t[0] != '{' {
			return body, nil, nil
		}
		// Skip the rewrite entirely when the body already carries the provider's
		// model id (the common case where the client requests the upstream model
		// name directly, e.g. no alias). sjson.SetBytes would otherwise rebuild
		// the whole ~50 KB body to write an identical value — a pure-waste
		// allocation on the hot path. GetBytes stops at the top-level model field,
		// so this no-op check does not scan the whole body.
		if gjson.GetBytes(body, "model").String() == req.Target.ProviderModelID {
			return body, nil, nil
		}
		out, err := sjson.SetBytes(body, "model", req.Target.ProviderModelID)
		if err != nil {
			return nil, nil, err
		}
		return out, nil, nil
	}
	// Streaming all-skip: a conformant streaming body — provider model already
	// set, stream:true, and stream_options.include_usage already present — needs
	// NO rewrite. Return it untouched (zero allocation) instead of paying the
	// map round-trip below just to re-emit byte-identical content. This is the
	// common shape when the client sends the upstream model name and already
	// requests usage (the OpenAI-stream SDK default). gjson scans + json.Valid
	// are alloc-free; malformed bodies are NOT skipped — they fall to the map
	// path's json.Unmarshal which preserves the 400 contract. A benchmark
	// confirmed map[string]any is the lowest-alloc round-trip when a rewrite IS
	// needed (it beats sjson and map[string]json.RawMessage), so the map path is
	// kept for that case — only the no-op case is short-circuited here.
	if req.Stream && req.WireShape == typology.WireShapeOpenAIChat && !rewriteMayApply {
		// No pre-validation (see the non-stream fast path above): a malformed body
		// is the client's problem. One parse for all three fields (GetManyBytes).
		// If all three already conform, forward verbatim; otherwise fall to the map
		// path, which both applies the usage option and rejects malformed bodies.
		r := gjson.GetManyBytes(body, "model", "stream", "stream_options.include_usage")
		if r[0].String() == req.Target.ProviderModelID && r[1].Exists() && r[2].Exists() {
			return body, nil, nil
		}
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
