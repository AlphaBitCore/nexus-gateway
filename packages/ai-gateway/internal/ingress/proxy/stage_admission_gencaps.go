package proxy

// stage_admission_gencaps.go — the generative-endpoint built-in caps arm of
// the admission stage (e88-s4 / NFR-4). Split out of stage_admission.go to keep
// that file under its size ratchet. Expensive generative endpoints (image
// today; tts/video/realtime as they land) carry a built-in per-VK concurrency
// bound the global admission gate lacks, so a single leaked/abusive VK cannot
// open unbounded concurrent per-call-priced requests.

import (
	"errors"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/generativecaps"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// errGenerativeBodyTooLarge distinguishes a 413 caused by the tighter per-kind
// generative body ceiling from the global-cap 413, so the caller emits a hint
// naming the right lever (the generative env var, not the global payload cap).
var errGenerativeBodyTooLarge = errors.New("request body exceeds the generative endpoint body ceiling")

// generativeJSONWireShape reports whether the ingress wire shape is a
// small-JSON generative request the per-kind body ceiling applies to. It
// deliberately excludes the multipart generative routes (images edits /
// variations), which share the image_generation kind but carry a multi-MB
// source image — they are not registered yet and must carry their own limit.
func generativeJSONWireShape(ws typology.WireShape) bool {
	switch ws {
	case typology.WireShapeOpenAIImages, typology.WireShapeOpenAIAudioSpeech:
		return true
	default:
		return false
	}
}

// admitGenerativeConcurrency enforces the per-(VK, kind) generative
// concurrency cap. Runs after auth+RPM (VK + endpoint kind known) and before
// the body read (an over-cap key cannot force full-body reads). Returns false
// after writing a 429 when the cap is hit; true (admitting) for non-generative
// kinds, uncapped kinds, and admitted requests. On admit it stashes the release
// on the state so finalizeAudit returns the slot on every exit path (the defer
// covers success, error, and panic-unwind).
func (h *Handler) admitGenerativeConcurrency(s *proxyState, vkMeta *vkauth.VKMeta) bool {
	kind := typology.KindFromWireShape(s.resolved.WireShape)
	caps, generative := generativecaps.Lookup(kind)
	if !generative || caps.MaxConcurrentPerVK <= 0 {
		return true
	}
	if h.genConcurrency != nil && !h.genConcurrency.Acquire(kind, vkMeta.ID, caps.MaxConcurrentPerVK) {
		generativeCapShedTotal.WithLabelValues(string(kind)).Inc()
		// Retry-After MUST be set before writeDetailedErr commits the header
		// block (a Header().Set after WriteHeader is a silent no-op on a real
		// ResponseWriter — the client's only back-off signal).
		s.w.Header().Set("Retry-After", "1")
		h.writeDetailedErr(s.w, s.rec, http.StatusTooManyRequests, "GENERATIVE_CONCURRENCY_LIMIT",
			"too many concurrent generative requests for this key",
			"Reduce concurrency or retry shortly")
		return false
	}
	vkID := vkMeta.ID
	s.releaseGenerativeCap = func() { h.genConcurrency.Release(kind, vkID) }
	return true
}
