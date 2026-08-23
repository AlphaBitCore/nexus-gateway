package proxy

import (
	"net/http"
	"strings"
	"testing"
)

// Every Nexus-stamped response carries the hop marker: nexus-headers.md states
// the minimum a response shows is X-Nexus-Via and X-Nexus-Request-Id, and the
// via chain's one job is telling a reader how many Nexus hops a response
// crossed — an agent-to-proxy-to-gateway path prepends one entry per hop.
//
// The transcription route is a parallel handler that never runs the ServeProxy
// stage chain, which is where PrependVia lives, so its responses arrived with
// no marker: measured against a live deployment, a transcription response
// carried x-nexus-request-id and the allowlisted upstream headers but no
// x-nexus-via, while a chat response on the same deployment carried it.
//
// The assertion goes through ServeSTT rather than calling the stamping helper,
// because the defect was never in the helper — it was that nothing called one.
func TestServeSTT_ResponseCarriesTheViaMarker(t *testing.T) {
	srv, _ := sttPromptCapturingUpstream(t, `{"text":"ok","usage":{"seconds":2}}`)
	deps, _ := sttDeps(t, srv.URL)
	h := NewHandler(deps)

	body, ct := buildSTTMultipartWithPrompt(t, "whisper-1", "", []byte("RIFFxxxx"))
	rr := doSTT(h.ServeSTT(sttIngress()), "/v1/audio/transcriptions", ct, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200", rr.Code, rr.Body.String())
	}

	via := rr.Header().Get("X-Nexus-Via")
	if via == "" {
		t.Fatal("no X-Nexus-Via on the transcription response — a reader cannot tell it crossed the gateway")
	}
	if !strings.Contains(via, "ai-gateway") {
		t.Errorf("X-Nexus-Via = %q, want it to name this hop", via)
	}
}

// The marker prepends, so a response that already crossed another Nexus hop
// keeps that hop's entry.
func TestServeSTT_ViaMarkerPrependsToAnExistingChain(t *testing.T) {
	h := http.Header{}
	h.Set("X-Nexus-Via", "compliance-proxy")
	stampSTTResponseMarkers(h)

	via := h.Get("X-Nexus-Via")
	if !strings.Contains(via, "compliance-proxy") {
		t.Errorf("X-Nexus-Via = %q, want the earlier hop preserved", via)
	}
	if !strings.HasPrefix(via, "ai-gateway") {
		t.Errorf("X-Nexus-Via = %q, want this hop prepended", via)
	}
}
