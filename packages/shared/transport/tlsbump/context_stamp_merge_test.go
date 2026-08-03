package tlsbump

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
)

// Finding C-3: the request phase's audit context and the CPMarker used to be two
// separate WithContext stamps, and therefore two http.Request clones, even though
// stampCPMarker runs immediately after the request phase with nothing reading the
// context in between. They now ride in one clone.
//
// The risk the merge carries is that one of the two silently stops arriving. Neither
// absence raises an error: a missing CPMarker means the response quietly loses its
// X-Nexus chain markers, and a missing audit context means the response-stage emit
// quietly loses the request-stage hook results from the audit row. So both are
// asserted together, on one request, at the observable end.

// TestForwardHandler_MergedContextStamp_CarriesBothValues drives a request with a
// request-stage hook and checks BOTH values survived the single clone.
func TestForwardHandler_MergedContextStamp_CarriesBothValues(t *testing.T) {
	writer := &recordingAuditWriter{}
	rt := &bodyCapturingRoundTripper{makeResp: jsonUpstream}
	areg := traffic.NewAdapterRegistry("test")
	if err := areg.Register("openai-compat", func() traffic.Adapter { return &openai.Adapter{} }); err != nil {
		t.Fatalf("adapter register: %v", err)
	}
	bo := &bumpOptions{
		policyResolver:  decidingResolver(t, "request", core.Approve, "ok", "OK"),
		auditEmitter:    compliance.NewAuditEmitter(writer, discardSlog()),
		domainEngine:    adapterDomainEngine(t, "openai-compat"),
		adapterRegistry: areg,
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	req := httptest.NewRequest(http.MethodPost, "https://api.example.com/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// The audit context reached the response-stage emit: the row carries the
	// request-stage decision, which is read from requestAuditCtx.
	events := writer.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
	if events[0].RequestHookDecision != string(core.Approve) {
		t.Fatalf("RequestHookDecision = %q, want %q. The request phase's audit context did not "+
			"reach the emit — it rides in the same clone as the CPMarker now, so a merge that "+
			"drops it loses request-stage hook results from every audit row, silently.",
			events[0].RequestHookDecision, core.Approve)
	}

	// The CPMarker reached the response write site: the chain markers are stamped.
	// Absence here is equally silent — the response is still correct, it just stops
	// carrying the correlation chain.
	if got := rec.Header().Get("X-Nexus-Request-Id"); got == "" {
		t.Fatalf("response headers = %v, want an X-Nexus-Request-Id from the CPMarker. "+
			"The marker did not reach the write site.", rec.Header())
	}
}

// TestForwardHandler_MergedContextStamp_NilAuditCtxOnFastPath covers the one case the
// merge introduced a nil check for: the compliance-disabled fast path stamps a marker
// without ever running a request phase, so x.auditCtx is nil there. It must stamp the
// marker anyway rather than skip it or panic — CPMarkerFromContext's contract is that
// the basic request-id field is always available to downstream writers.
func TestForwardHandler_MergedContextStamp_NilAuditCtxOnFastPath(t *testing.T) {
	rt := &bodyCapturingRoundTripper{makeResp: jsonUpstream}
	bo := &bumpOptions{
		// No policyResolver at all: compliance is disabled, so no request phase runs
		// and nothing sets x.auditCtx.
		domainEngine: singleDomainEngine(t, domain.PathActionProcess),
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	req := httptest.NewRequest(http.MethodPost, "https://api.example.com/v1/chat", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req) // must not panic

	// Not just "a response was written" — that assertion was inert. A mutation that
	// returns early when auditCtx is nil, skipping the marker stamp entirely, passed
	// the whole suite. stampCPMarker's contract is that the request-id field is
	// ALWAYS available to downstream writers, including here, so that is what is
	// asserted.
	if got := rec.Header().Get("X-Nexus-Request-Id"); got == "" {
		t.Fatalf("response headers = %v, want an X-Nexus-Request-Id on the compliance-disabled "+
			"fast path too. The merged stamp must not skip the marker just because there is no "+
			"audit context to ride along with it.", rec.Header())
	}
}
