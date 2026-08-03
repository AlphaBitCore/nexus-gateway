package dispatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
		// A codec Fail that is ALREADY a typed *ProviderError (its own Status /
		// Code / Type, e.g. a nexus-field rejection) survives verbatim — do not
		// re-wrap it into a generic 400, which would discard the codec's type
		// and mislabel a non-400 codec error. Only an untyped codec error
		// (plain fmt.Errorf: missing model, empty body) gets the generic 400.
		var pe *ProviderError
		if errors.As(err, &pe) {
			return nil, pe
		}
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
		// A structured *ProviderError from the codec's decode propagates
		// verbatim: it carries a deliberate status/code the codec chose for a
		// 2xx upstream body (e.g. the cross-shape image codec's content-policy
		// 400, whose CodeInvalidRequest keeps it out of retry/failover). Only
		// unstructured decode failures get the generic 502 wrap.
		var pe *ProviderError
		if errors.As(err, &pe) {
			return nil, pe
		}
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

// PrepareBody triages between the native leg (codec.RewriteNative) and the
// cross-format leg (codec.EncodeRequest). Returns the wire body, the list
// of in-place rewrites applied (empty when none), and any encoding error.
// Both legs can report rewrites: per-model rules live in the codec and
// fire from either entry point. Idempotent; no side effects.
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
	if a.nativeLeg(req) {
		return a.prepareNative(req)
	}
	// Cross-format leg: canonical OpenAI input needs codec translation.
	// Codecs may apply per-model rewrites of their own (sampling-param
	// strips, parameter renames) and surface them so the x-nexus-coerced
	// header reflects what the upstream actually saw.
	result, encErr := a.spec.SchemaCodec.EncodeRequest(req.WireShape, req.Body, req.Target)
	if encErr != nil {
		return nil, nil, "", encErr
	}
	return result.Body, result.Rewrites, result.URLOverride, nil
}

// nativeLeg is the same-spec triage: may this body skip the canonical
// round-trip and take the codec's RewriteNative differential instead? The
// decision is three keys, not format equality — a naive
// `BodyFormat == Format` check already shipped a 400 once (see
// prepareUpstreamBody's history in stage_cache_body.go):
//
//  1. true same format;
//  2. both sides OpenAI-family (the 12 compat siblings share the wire —
//     the model field must still be rewritten across
//     distinct-but-compatible formats, which is exactly the differential);
//  3. the Responses capability: a /v1/responses body headed to a target
//     that natively serves the Responses wire — formats differ
//     (FormatOpenAIResponses is not OpenAI-family), yet verbatim-plus-diff
//     is correct. The capability defaults from the adapter's declared
//     RequestShapes with the per-provider ServesResponsesAPI override on
//     top (same semantics as the ingress-side canonicalbridge decision, so
//     the cache-prep and executor legs cannot triage differently).
func (a *specAdapter) nativeLeg(req Request) bool {
	if req.BodyFormat == a.spec.Format {
		return true
	}
	if req.BodyFormat.IsOpenAIFamily() && a.spec.Format.IsOpenAIFamily() {
		return true
	}
	if req.BodyFormat == FormatOpenAIResponses && req.WireShape == typology.WireShapeOpenAIResponses {
		// Downgrade-only override, matching the canonicalbridge decision and
		// the field's own contract: an explicit false forces the canonical
		// leg; true cannot exceed the adapter's declared RequestShapes — an
		// operator flag must never route a verbatim Responses body onto a
		// wire whose codec cannot serve it.
		if req.Target.ServesResponsesAPI != nil && !*req.Target.ServesResponsesAPI {
			return false
		}
		return a.spec.SupportsShape(typology.WireShapeOpenAIResponses)
	}
	return false
}

// prepareNative runs the native leg: dispatch-owned guards, then the
// codec's same-spec differential. The codec is always in the path — what
// this leg skips is only the trip through the OpenAI canonical spec.
func (a *specAdapter) prepareNative(req Request) ([]byte, []string, string, error) {
	// Strip the gateway-internal `nexus` namespace before anything else.
	// The native leg forwards req.Body to upstream (modulo the codec's
	// differential) and no upstream understands the namespace — most 4xx
	// the request. The cross-format leg must NOT strip: its codecs CONSUME
	// nexus.ext.<provider>.<key> (canonicalext) during translation.
	body := stripNexusNamespace(req.Body)

	// Degenerate target: nothing to stamp and no quirk can key on an empty
	// model id. Preserved from the legacy passthrough.
	if req.Target.ProviderModelID == "" {
		return body, nil, "", nil
	}

	// Non-object carve-out — a stated dispatch-level exception to the
	// codec-always-in-path invariant: a JSON edit on a non-object body
	// (malformed garbage, a bare scalar/array) does not forward the
	// client's bytes, it FABRICATES a synthetic object. We would then send
	// an invented request upstream on the shared credential while the audit
	// row stored the client's original bytes (wire ≠ audit). Forward
	// verbatim and let the upstream return its own error. Leading-byte
	// scan only — no full validation pass (the upstream already validates).
	if t := bytes.TrimLeft(body, " \t\r\n"); len(t) == 0 || t[0] != '{' {
		return body, nil, "", nil
	}

	res, err := a.spec.SchemaCodec.RewriteNative(req.WireShape, body, req.Target, req.Stream)
	if err != nil {
		return nil, nil, "", err
	}
	return res.Body, res.Rewrites, res.URLOverride, nil
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
