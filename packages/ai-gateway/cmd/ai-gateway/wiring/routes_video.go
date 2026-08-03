// Package wiring — routes_video.go holds the async video route registrations,
// extracted from routes.go along the file-size ratchet seam (same pattern as
// routes_middleware.go). Behavior-identical to the inline block it replaced.
package wiring

import (
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/proxy"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// mountVideoRoutes registers the async video-generation family on the core
// mux. The multipart POST /v1/videos submit + its poll / content / delete
// follow-ups go through the PARALLEL async video handler family (ServeVideo*),
// NOT ServeProxy — the submit is a large multipart upload whose response is a
// JOB object, and the follow-ups are governed passthroughs keyed by a
// gateway-owned correlation store (see e88-s6). Submit + poll MUST mount
// together — mounting submit alone would strand every job (a client could
// submit but never poll). A Gemini-format target takes the Veo cross-shape
// leg inside the handlers (§3b).
func mountVideoRoutes(mux *http.ServeMux, proxyHandler *proxy.Handler) {
	videoIngress := proxy.Ingress{WireShape: typology.WireShapeOpenAIVideos, BodyFormat: provcore.FormatOpenAI}
	mux.HandleFunc("POST /v1/videos", proxyHandler.ServeVideoSubmit(videoIngress))
	mux.HandleFunc("GET /v1/videos/{id}", proxyHandler.ServeVideoPoll(videoIngress))
	mux.HandleFunc("GET /v1/videos/{id}/content", proxyHandler.ServeVideoContent(videoIngress))
	mux.HandleFunc("DELETE /v1/videos/{id}", proxyHandler.ServeVideoDelete(videoIngress))

	// Deliberately-unserved video sub-routes answer an OpenAI-shaped 404
	// envelope (not the mux's bare 404 an SDK can't tell from a wrong base
	// URL): list is account-wide (would leak other VKs' jobs — poll by id
	// instead); remix / edits / extensions / characters render additional
	// PAID seconds and enter scope only with their own admission-cost design.
	mux.HandleFunc("GET /v1/videos", proxyHandler.ServeVideoUnsupported(videoIngress,
		"VIDEO_LIST_UNSUPPORTED", "listing videos is not served",
		"Poll a specific job with GET /v1/videos/{id}; jobs are visible only to the key that submitted them"))
	for _, op := range []string{
		"POST /v1/videos/{id}/remix",
		"POST /v1/videos/edits",
		"POST /v1/videos/extensions",
		"POST /v1/videos/characters",
	} {
		mux.HandleFunc(op, proxyHandler.ServeVideoUnsupported(videoIngress,
			"VIDEO_OP_UNSUPPORTED", "this video operation is not served",
			"remix / edits / extensions / characters are not available through the gateway"))
	}
}
