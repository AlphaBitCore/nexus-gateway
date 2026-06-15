// stage_admission.go — the admission stage of the proxy stage chain:
// VK authentication, rate limiting, the bounded body read with model
// extraction, payload-capture stamping, and the canonical request
// context build. Owns proxyState.vkMeta / body / modelID / isStream /
// rctxFull.
package proxy

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

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

	// Phase 3: Read body (uses ingress format to pick the right
	// model-field source: JSON body for body-carrying formats,
	// URL path for Gemini/Azure). Runs only after auth + rate-limit
	// admission has passed.
	bodyReadStart := time.Now()
	body, modelID, isStream, err := h.readBody(s.r, s.resolved)
	s.phaseTimer.MarkBetween(traffic.PhaseBodyRead, time.Since(bodyReadStart))
	if err != nil {
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
	}

	// Phase 3.5: Build the canonical request context. One
	// normcore.Registry.Normalize call per request produces the
	// canonical *normcore.NormalizedPayload that L4 consumers
	// (routing first; hooks + audit follow in subsequent stories)
	// read instead of re-parsing raw bytes. The S1 RequestContext
	// type is the L3 immutable carrier; routing reads its Normalized()
	// via *routingcore.RoutingContext.Request.
	// Use resolved.BodyFormat (post-header-override), matching every
	// other consumer (rec.IngressFormat, canonicalization at the
	// upstream prep step). Using the pre-override in.BodyFormat here
	// would normalize a header-overridden cross-family body with the
	// wrong codec for the L3 RequestContext (smart routing / hooks /
	// semantic-cache pre-pass).
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
// stored on traffic_event_payload (inline JSONB vs spill file).
//
// To detect overflow without buffering the oversized body in memory we
// read up to `maxBytes + 1`; if the returned slice exceeds `maxBytes`,
// we return errRequestTooLarge so the caller can answer 413 cleanly.
func (h *Handler) readBody(r *http.Request, in Ingress) (body []byte, modelID string, isStream bool, err error) {
	maxBytes := h.payloadCaptureConfig().MaxRequestBytes
	if maxBytes <= 0 {
		maxBytes = payloadcapture.DefaultMaxRequestBytes
	}
	body, err = io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return nil, "", false, fmt.Errorf("failed to read request body")
	}
	if int64(len(body)) > maxBytes {
		return nil, "", false, errRequestTooLarge
	}

	modelID, isStream, err = ExtractIngressModel(in, r, body)
	if err != nil {
		return nil, "", false, err
	}

	if modelID == "" {
		return nil, "", false, fmt.Errorf("model is required")
	}

	if modelID == "auto" && typology.KindFromWireShape(in.WireShape) == typology.EndpointKindEmbeddings {
		return nil, "", false, fmt.Errorf("model \"auto\" is not supported for embeddings")
	}

	return body, modelID, isStream, nil
}
