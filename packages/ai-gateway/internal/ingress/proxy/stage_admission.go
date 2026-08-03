// stage_admission.go — the admission stage of the proxy stage chain:
// VK authentication, rate limiting, the bounded body read with model
// extraction, payload-capture stamping, and the canonical request
// context build. Owns proxyState.vkMeta / body / modelID / isStream /
// rctxFull.
package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/generativecaps"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// readBufPool reuses the request-body read scratch across requests. The body
// escapes to the async audit writer, so it cannot itself be pooled (a
// refcount/return-after-drain scheme was tried and reverted for live-object
// heap retention); instead the scratch is pooled and a right-sized body is
// copied out. Buffers are pre-grown to a typical body size so reading a payload
// does not pay bytes.Buffer's geometric regrowth (64K→128K→256K…, each step a
// full re-copy) or a fresh alloc per request. Pre-grown to 128 KiB: most current
// requests carry ~128K-token contexts, so a 64 KiB pre-grow forced at least one
// doubling+re-copy on the hot read path. This is a FIXED size, independent of
// the client Content-Length (which is untrusted — the read is bounded by
// LimitReader(maxBytes+1) below, never by a caller-supplied length).
// readBufPreGrowBytes is the per-buffer pre-grow size, configurable via
// server.requestReadBufKb (SetReadBufPreGrow, called once at wiring before any
// request). Defaults to 64 KiB — the conservative out-of-the-box fallback when
// unconfigured; deployments that routinely carry ~128K-token contexts raise it
// (e.g. 128) to avoid a bytes.Buffer doubling+re-copy on the read path. Read by
// the pool's New below, which runs lazily on first Get — i.e. after wiring — so
// the configured value is in effect.
var readBufPreGrowBytes = 64 << 10

// SetReadBufPreGrow sets the request-body read-scratch pre-grow size (bytes). A
// non-positive value keeps the current default. Call at startup before serving;
// it is not safe to change while requests are in flight.
func SetReadBufPreGrow(n int) {
	if n > 0 {
		readBufPreGrowBytes = n
	}
}

var readBufPool = sync.Pool{New: func() any {
	b := new(bytes.Buffer)
	b.Grow(readBufPreGrowBytes)
	return b
}}

// readBufPoolCap drops scratch buffers that grew past this cap rather than
// returning them to the pool (oversized-body guard).
const readBufPoolCap = 256 << 10

// admissionStage authenticates and admits the request before any body
// processing.
type admissionStage struct{ s *proxyState }

func (st admissionStage) run() bool {
	s := st.s
	h := s.h

	// Phase 1: VK Auth. Authenticate BEFORE reading or parsing the
	// full request body so an unauthenticated caller cannot force a
	// full MaxRequestBytes network read + JSON model extraction, and
	// — when StoreRequestBody is enabled — cannot get attacker-
	// controlled bytes persisted to the audit store. Auth depends only
	// on request headers (the VK token), not on the body, so it is the
	// correct first admission gate.
	vkMeta, err := h.authenticate(s.r)
	if err != nil {
		s.logger.Debug("auth failed", "error", err)
		h.writeAuthError(s.w, s.rec, err)
		return false
	}
	s.logger.Debug("auth ok", "vkName", vkMeta.Name, "orgId", vkMeta.OrganizationID)
	// Stamp VK ID on context for credential pool sticky routingcore.
	s.r = s.r.WithContext(withStickyKey(s.r.Context(), vkMeta.ID))
	s.rec.ApplyVKMeta(vkMeta)
	// Per-VK fingerprint for cost attribution without storing the
	// raw key. Class is empty for opaque slug tokens.
	s.rec.APIKeyClass = vkMeta.Class
	s.rec.APIKeyFingerprint = vkMeta.Fingerprint
	// Override UserID with VK owner's NexusUser ID for cross-path identity correlation.
	if vkMeta.OwnerID != "" {
		s.rec.UserID = vkMeta.OwnerID
		// UserDisplayName already set from VKMeta
	}
	s.vkMeta = vkMeta
	s.phaseTimer.Mark(traffic.PhaseAuth)

	// Phase 2: Rate limit. Throttle BEFORE the body read so a
	// rate-limited key cannot keep forcing full-body reads either.
	if err := h.checkRateLimit(s.w, vkMeta); err != nil {
		h.writeDetailedErr(s.w, s.rec, http.StatusTooManyRequests, "RATE_LIMITED",
			err.Error(), "Reduce request frequency or contact admin to increase limits")
		return false
	}
	// Set rate limit visibility headers.
	if vkMeta.RateLimitRpm != nil {
		s.w.Header().Set("X-RateLimit-Limit", strconv.Itoa(*vkMeta.RateLimitRpm))
	}

	// Phase 2b: Per-VK generative concurrency cap. Expensive generative
	// endpoints (image today; tts/video/realtime as they land) carry a
	// built-in per-VK concurrency bound the global admission gate lacks, so a
	// single leaked/abusive VK cannot open unbounded concurrent per-call-priced
	// requests (e88 NFR-4). Runs after auth+RPM (VK + endpoint kind are known)
	// and before the body read (an over-cap key cannot force full-body reads).
	// Non-generative kinds skip this entirely — Lookup returns not-generative.
	if !h.admitGenerativeConcurrency(s, vkMeta) {
		return false
	}

	// Phase 3: Read body (uses ingress format to pick the right
	// model-field source: JSON body for body-carrying formats,
	// URL path for Gemini/Azure). Runs only after auth + rate-limit
	// admission has passed.
	bodyReadStart := time.Now()
	body, bodyHandle, modelID, isStream, err := h.readBody(s.r, s.resolved)
	s.phaseTimer.MarkBetween(traffic.PhaseBodyRead, time.Since(bodyReadStart))
	if err != nil {
		if errors.Is(err, errGenerativeBodyTooLarge) {
			// The generative per-kind ceiling fired, NOT the global cap —
			// name the lever that actually governs it (raising the global
			// payload_capture cap would not help).
			h.writeDetailedErr(s.w, s.rec,
				http.StatusRequestEntityTooLarge,
				"PAYLOAD_TOO_LARGE",
				"request body exceeds this generative endpoint's body limit",
				"Reduce the request size; this endpoint enforces a tighter per-kind body ceiling (env AI_GATEWAY_GENERATIVE_CAP_<KIND>_MAX_BYTES)")
			return false
		}
		if errors.Is(err, errRequestTooLarge) {
			h.writeDetailedErr(s.w, s.rec,
				http.StatusRequestEntityTooLarge,
				"PAYLOAD_TOO_LARGE",
				"request body exceeds the configured network read cap",
				"Reduce the request size or ask an admin to raise payload_capture.maxRequestBytes")
			return false
		}
		h.writeError(s.w, s.rec, http.StatusBadRequest, err.Error())
		return false
	}
	s.body = body
	s.modelID = modelID
	s.isStream = isStream

	// Stamp the literal model string the client sent (e.g. "auto",
	// "gpt-4o") on the audit record's "requested" side immediately —
	// before routing rewrites the picked target. ProviderID/Name and
	// ModelID stay empty: OpenAI-style clients don't pin a provider,
	// and the catalog UUID is a server-side concept. Routed* gets
	// filled by the cache-HIT and fetchUpstream paths from the
	// resolved RoutingTarget. Metrics + quota + cost math read the
	// resolved target directly and are not affected by this field.
	s.rec.ModelName = modelID

	// Caller-declared correlation tags. End-user: header first, then the
	// ingress protocol's native field; session: header only. Both opaque —
	// persisted onto traffic_event.{end_user_id,session_id} and read by
	// nothing else.
	s.rec.EndUserID = extractEndUserID(s.r.Header)
	s.rec.SessionID = extractSessionID(s.r.Header)

	// Snapshot the payload-capture config once per request so the
	// pre-hook request body and later response body decisions stay
	// consistent even if the admin invalidates mid-flight (Q2=A:
	// we store "what the caller sent", not any hook-modified bytes).
	// The full body is handed to the audit Writer; spillstore.EmitBody
	// decides inline (size <= MaxInlineBodyBytes) vs spill (>) at
	// flush time. The forwarded bytes are independently bounded by
	// MaxRequestBytes (already applied to `body` above).
	pcCfg := h.payloadCaptureConfig()
	if pcCfg.StoreRequestBody && len(body) > 0 {
		s.rec.RequestBody = body
		s.rec.RequestContentType = s.r.Header.Get("Content-Type")
		// The captured body IS the pooled buffer; hand its handle to the record so
		// the audit writer returns it to the pool at terminal resolution.
		s.rec.AttachPooledRequestBody(bodyHandle)
	} else {
		// Not captured: the record never references the buffer, so no terminal
		// reclaim will run. Stash the handle; finalizeAudit returns it to the
		// pool after the body's last reader (upstream forward, hooks, lazy
		// normalize — all complete before that defer) has finished. Dropping
		// it instead would re-allocate the pooled body buffer on every
		// request instead of reusing it.
		s.unattachedBodyHandle = bodyHandle
	}

	// Phase 3.5: Build the canonical request context. One
	// normcore.Registry.Normalize call per request produces the
	// canonical *normcore.NormalizedPayload that L4 consumers
	// (routing first; hooks + audit follow in subsequent stories)
	// read instead of re-parsing raw bytes. The RequestContext
	// type is the L3 immutable carrier; routing reads its Normalized()
	// via *routingcore.RoutingContext.Request.
	// Use resolved.BodyFormat, matching every other consumer
	// (rec.IngressFormat, canonicalization at the upstream prep step), so
	// the L3 RequestContext that smart routing / hooks / the semantic-cache
	// pre-pass read is decoded with the same codec they all assume.
	s.rctxFull = h.buildRequestContext(s.r, vkMeta, body, s.resolved.BodyFormat, modelID, s.endpointType)
	return true
}

// errRequestTooLarge is returned by readBody when the inbound body
// exceeds payloadcapture.MaxRequestBytes. The admission stage maps this
// to `413 Payload Too Large` instead of the generic 400 path so admins can
// distinguish a malformed request from one that simply outgrew the
// network read cap.
var errRequestTooLarge = errors.New("request body exceeds the configured network read cap")

// readBody reads the request body, extracts the client-requested
// model, and determines the stream flag. Model and stream sources are
// format-specific (path params for Gemini/Azure, body `model` for
// body-carrying formats) and resolved via [ExtractIngressModel].
//
// endpointType is used to reject model="auto" for non-chat endpoints.
// The network read cap is taken from the runtime payload-capture store
// (`MaxRequestBytes`, default 10 MiB) so admin edits take effect on the
// very next request without a restart. A non-positive store value
// falls back to the package default so a stale or malformed config
// never collapses the read to zero (which would otherwise 413 every
// inbound request). The inline-vs-spill cutoff (`MaxInlineBodyBytes`)
// is NOT applied here — it only governs how the captured copy is
// stored on traffic_event_payload (inline BYTEA vs spill file).
//
// To detect overflow without buffering the oversized body in memory we
// read up to `maxBytes + 1`; if the returned slice exceeds `maxBytes`,
// we return errRequestTooLarge so the caller can answer 413 cleanly.
func (h *Handler) readBody(r *http.Request, in Ingress) (body []byte, bodyHandle *[]byte, modelID string, isStream bool, err error) {
	maxBytes := h.payloadCaptureConfig().MaxRequestBytes
	if maxBytes <= 0 {
		maxBytes = payloadcapture.DefaultMaxRequestBytes
	}
	// Generative endpoints carry a tighter per-kind body ceiling (their bodies
	// are small JSON prompts; the global 10 MiB cap is far too loose to shut a
	// body-flood against a per-call-priced endpoint). Clamp to the smaller of
	// the two — an oversized generative body 413s at the tighter ceiling via
	// the existing errRequestTooLarge path, and clampedGenerative flags it so
	// the 413 hint names the right lever.
	//
	// The size clamp is gated on the JSON wire shapes only. The multipart
	// generative routes (/v1/images/edits, /v1/images/variations) share the
	// image_generation KIND but carry a multi-MB source image, so a
	// kind-keyed byte ceiling would wrongly 413 them the day they land — those
	// routes are not registered yet (they 404), and when they are, they must
	// carry their own (larger) multipart size limit, not this JSON ceiling.
	clampedGenerative := false
	if generativeJSONWireShape(in.WireShape) {
		if caps, generative := generativecaps.Lookup(typology.KindFromWireShape(in.WireShape)); generative &&
			caps.MaxRequestBytes > 0 && caps.MaxRequestBytes < maxBytes {
			maxBytes = caps.MaxRequestBytes
			clampedGenerative = true
		}
	}
	// Read into a POOLED scratch buffer (pre-grown to ~64 KB) so the common
	// ~50 KB body neither pays io.ReadAll's geometric regrowth nor a fresh
	// per-request buffer allocation. The body escapes to the async audit writer,
	// so a right-sized copy is taken out of the scratch and the scratch is
	// returned to the pool — severing the async-ownership coupling without
	// retaining the (oversized) scratch past the request.
	buf := readBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	// Return to the pool only if it did not balloon on an oversized body — one
	// 10 MiB request must not inflate every pooled scratch thereafter.
	defer func() {
		if buf.Cap() <= readBufPoolCap {
			readBufPool.Put(buf)
		}
	}()
	if _, err = buf.ReadFrom(io.LimitReader(r.Body, maxBytes+1)); err != nil {
		return nil, nil, "", false, fmt.Errorf("failed to read request body")
	}
	if int64(buf.Len()) > maxBytes {
		if clampedGenerative {
			return nil, nil, "", false, errGenerativeBodyTooLarge
		}
		return nil, nil, "", false, errRequestTooLarge
	}
	// Right-sized escaping copy taken from a POOL: the body escapes to the async
	// audit writer, which returns the buffer to the pool at the record's terminal
	// resolution (severing the per-request fresh allocation, the #1 hot-path
	// allocator). bodyHandle is the pool handle; the caller attaches it to the
	// audit Record when the body is captured, stashes it for the finalizeAudit
	// release when not, and the reject paths below return it directly — a
	// malformed-body storm must not bypass the pool and re-allocate per request.
	body, bodyHandle = audit.AcquireRequestBody(buf.Bytes())

	modelID, isStream, err = ExtractIngressModel(in, r, body)
	if err != nil {
		audit.ReleaseRequestBuffer(bodyHandle)
		return nil, nil, "", false, err
	}

	// Multimodal routes are handled non-stream only in this slice: cost
	// metering (stampModalityUnits) and the artifact fingerprint
	// (buildArtifactRefs) live exclusively on the non-stream response path.
	// A client-sent `stream: true` (e.g. gpt-image-1 partial_images) would
	// otherwise take the streaming path and skip both — silently pricing the
	// request at $0 with a NULL artifact_ref. Force non-stream; the raw body
	// is still forwarded verbatim and the upstream returns a non-stream
	// response. Streaming multimodal ships with its own accounting later.
	switch typology.KindFromWireShape(in.WireShape) {
	case typology.EndpointKindImageGeneration, typology.EndpointKindTTS:
		isStream = false
	}

	if modelID == "" {
		audit.ReleaseRequestBuffer(bodyHandle)
		return nil, nil, "", false, fmt.Errorf("model is required")
	}

	if modelID == "auto" && typology.KindFromWireShape(in.WireShape) == typology.EndpointKindEmbeddings {
		audit.ReleaseRequestBuffer(bodyHandle)
		return nil, nil, "", false, fmt.Errorf("model \"auto\" is not supported for embeddings")
	}

	return body, bodyHandle, modelID, isStream, nil
}
