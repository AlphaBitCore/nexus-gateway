package proxy

// stt_handler.go — the parallel STT (speech-to-text) streaming-proxy handler
// for the multipart /v1/audio/transcriptions and /v1/audio/translations routes.
//
// STT does NOT go through the small-JSON ServeProxy pipeline (see the e88-s5
// SDD "Architecture decision"): its request is a large binary multipart upload
// that the response cache (O(bytes) key, near-zero hit, cached-transcript PII),
// the text-scanning hook pipeline (audio is unscannable), and the
// canonical/codec bridge (native passthrough, no cross-shape) do not fit, and
// that forcing into the shared byte-slice executor would pollute for an edge
// modality. Instead this handler is a straight line —
// admission → bounded multipart parse → single-target resolve → native
// multipart forward → meter → audit — reusing the SAME cross-cutting subset
// (VK auth, per-VK RPM, per-VK generative-caps concurrency, router + credential
// resolve, cost estimator, audit writer) via the shared Handler methods and
// deps, and touching NONE of ServeProxy's cache/hook/codec/executor internals.
// provcore.Request, the executor, and spec_adapter are UNCHANGED.
//
// v1a is competitor-parity passthrough (LiteLLM / Portkey / TrueFoundry all
// passthrough, none redacts): the transcript forwards unredacted and
// compliance_coverage = none. Output transcript redaction (the differentiator)
// is v1b (SDD T4).

import (
	"bytes"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/estimator"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/sttproxy"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/generativecaps"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provdispatch "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/dispatch"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// sttMaxResponseBytes bounds the transcript body the handler buffers before
// metering + relay. Transcripts (json / verbose_json / text) are small — orders
// of magnitude smaller than the audio input; 32 MiB is a defensive ceiling so a
// misbehaving upstream cannot force an unbounded read on this path.
const sttMaxResponseBytes = 32 << 20

// authApplier is the optional interface a provider adapter satisfies to expose
// its per-provider auth scheme to a hand-built forward request. The concrete
// specAdapter implements it (delegating to Transport.ApplyAuth); it is NOT on
// the Adapter interface, so no test double of Adapter has to grow it.
type authApplier interface {
	ApplyAuth(*http.Request, provcore.CallTarget) error
}

// ServeSTT returns the HTTP handler for an STT route. The Ingress carries the
// STT wire shape + body format; the ingress request PATH (transcriptions vs
// translations) is preserved verbatim to the upstream (both share one wire
// shape, so the path is the only discriminator — gap #6), so a single handler
// serves both routes.
// stampSTTResponseMarkers puts this hop on the response.
//
// The transcription route is a parallel handler: it never runs the ServeProxy
// stage chain, which is where PrependVia lives, so without this its responses
// carried the allowlisted upstream headers and no sign they had crossed the
// gateway at all. nexus-headers.md states the minimum every Nexus-stamped
// response shows, and a missing hop marker breaks the via chain's one job —
// telling a reader how many Nexus hops a response actually made.
//
// Prepend, not set: a request that reached here through the compliance proxy
// or an agent already carries their entries, and the chain reads newest-first.
func stampSTTResponseMarkers(h http.Header) {
	traffic.PrependVia(h, "ai-gateway")
}

func (h *Handler) ServeSTT(in Ingress) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// In-flight admission gate: reject-fast before any per-request setup,
		// identical to ServeProxy's wrapper (no state, no audit row — the shed
		// path's observability is the admission_shed_total counter).
		if h.gate != nil {
			if !h.gate.acquire() {
				writeOverloaded(w, in.BodyFormat)
				return
			}
			defer h.gate.release()
		}

		start := time.Now().UTC()
		requestID := r.Header.Get("X-Nexus-Request-Id")
		rec := &audit.Record{
			RequestID:       requestID,
			ClientRequestID: r.Header.Get("x-request-id"),
			TraceID:         requestID,
			Timestamp:       start,
			Method:          r.Method,
			Path:            r.URL.Path,
			SourceIP:        middleware.ClientIP(r),
			IngressFormat:   string(in.BodyFormat),
			EndpointType:    string(typology.EndpointKindSTT),
		}
		stampCallerAttribution(rec, r.Header)

		// Owned, panic-safe audit tail (SDD A5): the generative-caps slot
		// release and the audit enqueue run on EVERY exit path — success,
		// error, and panic-unwind — exactly what ServeProxy.finalizeAudit's
		// defer guarantees. A leaked STT slot on an upstream panic would
		// wedge the per-VK cap.
		var releaseCap func()
		defer func() {
			if releaseCap != nil {
				releaseCap()
			}
			if h.deps.AuditWriter != nil {
				h.finalize(rec, start)
			}
		}()

		// VK auth.
		vkMeta, err := h.authenticate(r)
		if err != nil {
			h.writeAuthError(w, rec, err)
			return
		}
		rec.ApplyVKMeta(vkMeta)

		// Per-VK RPM rate limit (sets Retry-After on rejection).
		if err := h.checkRateLimit(w, vkMeta); err != nil {
			h.writeDetailedErr(w, rec, http.StatusTooManyRequests, "RATE_LIMITED",
				"per-key rate limit exceeded", "Reduce request rate or retry after the Retry-After interval")
			return
		}

		// Per-(VK, stt) generative-caps concurrency. Re-done here on the SAME
		// Handler.genConcurrency instance the JSON admitGenerativeConcurrency
		// uses (that method gates JSON wire shapes and excludes multipart — A4),
		// so per-VK STT caps count against the same buckets. Release is wired
		// into the panic-safe tail above.
		caps, generative := generativecaps.Lookup(typology.EndpointKindSTT)
		if generative && caps.MaxConcurrentPerVK > 0 && h.genConcurrency != nil {
			if !h.genConcurrency.Acquire(typology.EndpointKindSTT, vkMeta.ID, caps.MaxConcurrentPerVK) {
				generativeCapShedTotal.WithLabelValues(string(typology.EndpointKindSTT)).Inc()
				// Retry-After MUST precede writeDetailedErr's header commit
				// (a Set after WriteHeader is a silent no-op on a real writer).
				w.Header().Set("Retry-After", "1")
				h.writeDetailedErr(w, rec, http.StatusTooManyRequests, "GENERATIVE_CONCURRENCY_LIMIT",
					"too many concurrent generative requests for this key",
					"Reduce concurrency or retry shortly")
				return
			}
			vkID := vkMeta.ID
			releaseCap = func() { h.genConcurrency.Release(typology.EndpointKindSTT, vkID) }
		}

		// Multipart boundary. A non-multipart Content-Type is a malformed STT
		// request (400) — the body must be multipart/form-data.
		mediaType, params, mtErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mtErr != nil || !strings.HasPrefix(mediaType, "multipart/") || params["boundary"] == "" {
			h.writeDetailedErr(w, rec, http.StatusBadRequest, "STT_BAD_MULTIPART",
				"request Content-Type must be multipart/form-data with a boundary",
				"Send the audio file as multipart/form-data, e.g. curl -F file=@audio.mp3 -F model=whisper-1")
			return
		}

		// Bound the total upload mid-stream BEFORE parsing (the caps ceiling,
		// ~26 MiB) so a chunked / lying-Content-Length upload cannot force an
		// unbounded read. ParseSTTMultipart adds the per-part / per-field
		// structural bounds on top.
		maxBytes := caps.MaxRequestBytes
		if maxBytes <= 0 {
			maxBytes = 26 << 20
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

		sttReq, parseErr := sttproxy.ParseSTTMultipart(r.Body, params["boundary"])
		if parseErr != nil {
			status, code, message, hint := sttParseErrorResponse(parseErr)
			h.writeDetailedErr(w, rec, status, code, message, hint)
			return
		}

		// Response-format gate: v1 serves json / verbose_json / text. Subtitle
		// (srt/vtt) + streaming formats are deferred behind a fail-closed reader
		// / an SSE arm with no named consumer; a request for one is an explicit
		// 400, never a silent unredacted egress.
		if !sttFormatSupported(sttReq.ResponseFormat) {
			h.writeDetailedErr(w, rec, http.StatusBadRequest, "STT_FORMAT_UNSUPPORTED",
				"response_format not yet supported: "+sttReq.ResponseFormat,
				"Use response_format json, verbose_json, or text (srt/vtt/streaming are not yet supported)")
			return
		}
		// SSE streaming (the transcribe models' `stream=true` form field, a
		// SEPARATE trigger from response_format) is deferred in v1a: the handler
		// buffers the whole response, so a streamed body would be mis-metered
		// ($0, no usage in the SSE frames) and defeat the buffered relay. Reject
		// it explicitly rather than silently forwarding + mis-pricing.
		if sttStreamRequested(sttReq.Stream) {
			h.writeDetailedErr(w, rec, http.StatusBadRequest, "STT_FORMAT_UNSUPPORTED",
				"streaming transcription (stream=true) is not yet supported",
				"Omit stream or set stream=false; streaming STT is deferred")
			return
		}

		// The audio fingerprint is the INPUT artifact, recorded whether or not
		// the bytes are captured — it is what proves which file was sent.
		rec.ArtifactRefs = sttReq.ArtifactRefsJSON()

		h.captureSTTAudio(rec, sttReq)
		// No output scanning yet; coverage starts none and scanSTTPrompt
		// upgrades it to prompt-only when a content hook cleanly scanned the
		// prompt field (the one request-side text leaf — audio stays
		// fingerprint-only).
		rec.ComplianceCoverage = "none"

		// Request-stage compliance scan of the prompt field: block → 403,
		// redact → the sanitized prompt replaces the original before ReEmit,
		// approve → forward unchanged.
		if h.scanSTTPrompt(w, r, rec, sttReq, rec.RequestID) {
			return
		}

		// Route: resolve the requested model → provider target via the shared
		// router. No request body to normalize (audio is not a text leaf), so
		// the canonical stays nil and non-smart routing selects on metadata.
		rctx := h.buildRequestContext(r, vkMeta, nil, in.BodyFormat, sttReq.Model, string(typology.EndpointKindSTT))
		routeRes, routeErr := h.resolveRouteOrPassthrough(r.Context(), rctx, in, sttReq.Model, typology.EndpointKindSTT)
		if routeErr != nil || routeRes == nil || len(routeRes.AllTargets()) == 0 {
			// 404, NOT 502: an unknown / unconfigured / not-allowed-for-this-VK
			// model is a CLIENT-correctable condition (parity with the chat
			// no-match path). A 5xx would make an OpenAI SDK retry a request that
			// can never succeed.
			h.writeDetailedErr(w, rec, http.StatusNotFound, "NO_COMPATIBLE_PROVIDER",
				"no provider is configured to serve this stt model",
				"Check the model name, or add a provider + model mapping for it")
			return
		}
		target0 := routeRes.Primary()
		rec.ModelID = target0.ModelID
		rec.ModelName = target0.ModelName
		rec.RoutedModelID = target0.ModelID
		rec.RoutedModelName = target0.ModelName
		rec.RoutedProviderID = target0.ProviderID
		rec.RoutedProviderName = target0.ProviderName

		// Resolve the single upstream target (BaseURL + decrypted key + Format +
		// provider model id) via the SAME resolver the executor holds. STT
		// forwards to ONE target with NO failover in v1a — a deliberate
		// simplification, NOT a hard constraint: v1a bounded-buffers the audio
		// (capped by the MaxBytesReader ceiling), so the body IS re-readable and
		// failover is reachable; it is deferred because a wedged-credential retry
		// also wants circuit-breaker feedback the executor owns (signed residual
		// A6). An upstream failure returns the error to the caller.
		// 502 says the upstream failed. Nothing here has reached an upstream:
		// the resolver is a dependency of OURS that was never wired, and the
		// code claimed nothing was compatible when nothing was even asked.
		// Reporting it as a gateway fault also polluted every dashboard that
		// counts 502s as provider unavailability.
		if h.deps.Resolver == nil {
			h.writeDetailedErr(w, rec, http.StatusServiceUnavailable, "STT_RESOLVER_UNAVAILABLE",
				"speech-to-text target resolver is not configured on this gateway",
				"This is a gateway-side configuration gap, not a provider failure — contact an operator")
			return
		}
		callTarget, resolveErr := h.deps.Resolver.Resolve(r.Context(), target0.ProviderID, target0.ModelID,
			provtarget.ResolveHints{StickyKey: vkMeta.ID})
		if resolveErr != nil {
			// Resolving a credential is a lookup against our own configuration.
			// It fails before any request leaves the gateway, so it is not an
			// upstream fault.
			h.writeDetailedErr(w, rec, http.StatusServiceUnavailable, "PROVIDER_RESOLVE_FAILED",
				"could not resolve the upstream provider credential",
				"This is a gateway-side configuration problem — check the provider credential configuration")
			return
		}
		rec.ProviderID = callTarget.ProviderID
		rec.ProviderName = callTarget.ProviderName
		rec.CredentialID = callTarget.CredentialID
		rec.CredentialName = callTarget.CredentialName

		// Re-emit the multipart body with the model field rewritten to the
		// resolved provider model id (a client alias / routing-changed model
		// would otherwise 404 upstream).
		fwdBody, fwdContentType, emitErr := sttReq.ReEmit(callTarget.ProviderModelID)
		if emitErr != nil {
			h.writeDetailedErr(w, rec, http.StatusInternalServerError, "STT_REEMIT_FAILED",
				"failed to rebuild the multipart body for forwarding", "Retry; contact an operator if it persists")
			return
		}

		// Build the upstream request by hand (whitelist-by-omission: only
		// allowlisted client headers are copied, so client Authorization /
		// Cookie / x-api-key / x-nexus-* cannot reach the upstream). STT is
		// native passthrough (the resolved provider speaks the OpenAI audio
		// shape — non-OpenAI STT cross-shape is deferred to P5); the upstream
		// path is the ingress path (transcriptions vs translations preserved —
		// gap #6), so TrimRight(BaseURL) + path is the OpenAI-audio URL.
		upstreamURL := strings.TrimRight(callTarget.BaseURL, "/") + r.URL.Path
		upReq, reqErr := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(fwdBody))
		if reqErr != nil {
			// The request could not be constructed, which means our own
			// configured base URL is malformed. Nothing was sent, and a retry
			// cannot help — 500 rather than a 502 blaming a provider we never
			// contacted. Matches STT_REEMIT_FAILED above, the other
			// construction failure on this path.
			h.writeDetailedErr(w, rec, http.StatusInternalServerError, "STT_UPSTREAM_BUILD_FAILED",
				"failed to build the upstream request from the configured base URL",
				"This is a gateway-side configuration problem — check the provider base URL")
			return
		}
		// Copy the allowlisted client headers (provider betas / tracing) FIRST,
		// then Set the Content-Type — Set replaces the whole key, so the fresh
		// re-emit boundary is the SINGLE Content-Type on the forward. Order
		// matters: `content-type` is an allowlisted base header, so applying it
		// AFTER the Set would append the client's stale-boundary value (it does
		// not match the re-emitted body) as a second Content-Type line. This
		// mirrors the chat adapter's forwardHeaders-then-Set order.
		forwardheader.Apply(upReq.Header, r.Header, h.deps.Allowlist, string(callTarget.Format))
		upReq.Header.Set("Content-Type", fwdContentType)

		// Provider auth via the resolved adapter's scheme (Bearer / api-key /
		// x-goog-api-key), reused from the chat path (no second implementation).
		adapter, ok := h.deps.ProviderReg.Get(callTarget.Format)
		if !ok {
			h.writeDetailedErr(w, rec, http.StatusBadGateway, "NO_COMPATIBLE_PROVIDER",
				"no adapter registered for the resolved provider format", "Contact an operator")
			return
		}
		auth, ok := adapter.(authApplier)
		if !ok {
			h.writeDetailedErr(w, rec, http.StatusBadGateway, "PROVIDER_AUTH_UNAVAILABLE",
				"resolved provider adapter cannot apply auth to an stt forward", "Contact an operator")
			return
		}
		if authErr := auth.ApplyAuth(upReq, callTarget); authErr != nil {
			h.writeDetailedErr(w, rec, http.StatusBadGateway, "PROVIDER_AUTH_FAILED",
				"failed to apply provider authentication", "Check the provider credential")
			return
		}
		rec.TargetMethod = http.MethodPost
		rec.TargetPath = r.URL.Path
		rec.TargetHost = hostOf(callTarget.BaseURL)

		// Forward.
		resp, doErr := h.deps.UpstreamClient.Do(upReq)
		if doErr != nil {
			h.writeDetailedErr(w, rec, http.StatusBadGateway, "UPSTREAM_ERROR",
				"upstream provider request failed", "Retry; the provider may be unavailable")
			return
		}
		defer func() { _ = resp.Body.Close() }()

		respBody, _ := readResponseBounded(resp, sttMaxResponseBytes)
		rec.StatusCode = resp.StatusCode

		// Meter on success: usage tokens win (token-priced transcribe models such
		// as gpt-4o-transcribe report usage); AudioSeconds (verbose_json
		// duration) is the per-second-model fallback; neither present → the
		// underivable-units WARN (never a silent $0).
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			h.meterSTT(rec, respBody, target0.ModelID, r)
		}

		// Capture the transcript response so the Traffic drawer can render it
		// (view-time normalize → ai-stt). Audio request bytes stay uncaptured
		// (R-7). Gated on the payload-capture toggle.
		h.captureParallelResponse(rec, respBody, resp.Header.Get("Content-Type"))

		// Relay: allowlisted response headers, upstream status, transcript body.
		stampSTTResponseMarkers(w.Header())
		filtered := provdispatch.FilterResponseHeaders(h.deps.Allowlist, callTarget.Format, resp.Header, false)
		for k, vs := range filtered {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		if ct := resp.Header.Get("Content-Type"); ct != "" && w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
	}
}

// meterSTT stamps the STT billable units + cost onto rec from a 2xx transcript
// response. Usage tokens (when the provider reports them) win via the shared
// sttCostFormula; AudioSeconds (verbose_json duration) is the per-second-model
// fallback. The model's per-1M-billable-unit input price comes from the Model
// catalog (the same source the quota stage reads).
func (h *Handler) meterSTT(rec *audit.Record, respBody []byte, modelID string, r *http.Request) {
	units := estimator.BillableUnits{}
	// Provider usage tokens (gpt-4o-transcribe reports usage.{input,output}_tokens
	// or usage.{prompt,completion}_tokens depending on the shape).
	if pt := sttUsageTokens(respBody, "prompt_tokens", "input_tokens"); pt > 0 {
		units.PromptTokens = pt
		rec.PromptTokens = int64(pt)
	}
	if ct := sttUsageTokens(respBody, "completion_tokens", "output_tokens"); ct > 0 {
		units.CompletionTokens = ct
		rec.CompletionTokens = int64(ct)
	}
	rec.TotalTokens = rec.PromptTokens + rec.CompletionTokens
	// AudioSeconds fallback (per-second whisper-shape models report `duration`
	// on verbose_json). No duration + no tokens → underivable → WARN.
	if secs, ok := sttAudioSeconds(respBody); ok {
		units.AudioSeconds = secs
	}
	if warnUnderivableModalityUnits(string(typology.EndpointKindSTT), rec.StatusCode, units) {
		// The byte-count of the input audio is the diagnostic context: a
		// text/json caller who wants exact metering should request
		// verbose_json (the SDD Metering contract). We do NOT price the
		// byte-count as seconds — that would mis-bill catastrophically; an
		// honest WARN-plus-$0 is correct, and v1a's cost is duration-or-tokens.
		warnUnderivableUnitsOnce(h.deps.Logger, string(typology.EndpointKindSTT), modelID, false)
	}

	var inPrice, outPrice float64
	if h.deps.Models != nil {
		if m, mErr := h.deps.Models.GetModel(r.Context(), modelID); mErr == nil && m != nil {
			if m.InputPricePM != nil {
				inPrice = *m.InputPricePM
			}
			if m.OutputPricePM != nil {
				outPrice = *m.OutputPricePM
			}
		}
	}
	cost := estimator.Lookup(string(typology.EndpointKindSTT))(units, metrics.ModelPrices{
		InputUsdPerM:  &inPrice,
		OutputUsdPerM: &outPrice,
	})
	rec.EstimatedCostUsd = cost.Total
}

// sttAudioSeconds extracts the billable audio duration (seconds) from a
// transcript response. Two provider shapes carry it: verbose_json puts a
// top-level numeric `duration`, and the current OpenAI transcription shape
// (returned for json AND verbose_json) reports it under a usage object as
// {"usage":{"type":"duration","seconds":N}}. Both are read so a per-second
// whisper-shape model is not silently priced $0. Returns (0, false) when
// neither is present so the caller can WARN rather than price $0 silently.
func sttAudioSeconds(respBody []byte) (float64, bool) {
	if d := gjson.GetBytes(respBody, "duration"); d.Exists() && d.Type == gjson.Number {
		if v := d.Float(); v > 0 {
			return v, true
		}
	}
	// usage.seconds is the duration-typed usage shape; guard on the type so a
	// token-typed usage object (gpt-4o-transcribe) is not misread as seconds.
	if u := gjson.GetBytes(respBody, "usage"); u.Exists() {
		if t := u.Get("type").String(); t == "duration" || t == "" {
			if s := u.Get("seconds"); s.Exists() && s.Type == gjson.Number {
				if v := s.Float(); v > 0 {
					return v, true
				}
			}
		}
	}
	return 0, false
}

// sttUsageTokens reads the first present of the given usage sub-keys under a
// top-level `usage` object (transcribe models vary: {prompt,completion}_tokens
// vs {input,output}_tokens). Returns 0 when none is present.
func sttUsageTokens(respBody []byte, keys ...string) int {
	for _, k := range keys {
		if v := gjson.GetBytes(respBody, "usage."+k); v.Exists() && v.Type == gjson.Number {
			if n := int(v.Int()); n > 0 {
				return n
			}
		}
	}
	return 0
}

// hostOf returns the host component of a provider base URL for the audit
// target_host field, tolerating a scheme-less or malformed value.
func hostOf(baseURL string) string {
	s := baseURL
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}
