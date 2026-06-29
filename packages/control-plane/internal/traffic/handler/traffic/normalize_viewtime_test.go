package traffic

import (
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/store/trafficstore"
	sharednormalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// newNormalizeHandler builds a Handler with only the view-time normalize chain
// wired — enough to exercise computeNormalized without a database.
func newNormalizeHandler() *Handler {
	return &Handler{normalize: normcore.BuildAuditFn(sharednormalize.BuildRegistry(), nil)}
}

// TestComputeNormalized_OpenAIChatRequest verifies the view-time recompute turns
// a captured OpenAI chat request body into a canonical normalized payload that
// actually carries the user's message text — i.e. real normalization, not an
// empty/padding result — and reports status "ok".
func TestComputeNormalized_OpenAIChatRequest(t *testing.T) {
	h := newNormalizeHandler()
	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello view-time normalize"}]}`)
	in := &trafficstore.NormalizeInput{
		Found:              true,
		AdapterType:        "openai",
		Model:              "gpt-4o-mini",
		Path:               "/v1/chat/completions",
		RequestContentType: "application/json",
		RequestBody:        body,
	}

	out := h.computeNormalized("evt-1", in)

	if out.TrafficEventID != "evt-1" {
		t.Fatalf("trafficEventId = %q, want evt-1", out.TrafficEventID)
	}
	if out.NormalizeVersion == "" {
		t.Fatal("normalizeVersion must be stamped")
	}
	if len(out.RequestNormalized) == 0 {
		t.Fatal("requestNormalized is empty — recompute produced nothing")
	}
	if out.RequestStatus == nil || *out.RequestStatus != "ok" {
		t.Fatalf("requestStatus = %v, want ok", out.RequestStatus)
	}
	if !strings.Contains(string(out.RequestNormalized), "hello view-time normalize") {
		t.Fatalf("normalized payload missing the user message text: %s", out.RequestNormalized)
	}
	// Request-only input must not fabricate a response side.
	if len(out.ResponseNormalized) != 0 || out.ResponseStatus != nil {
		t.Fatalf("response fields should be unset for a request-only input")
	}
}

// TestComputeNormalized_ResponseStreamFlag verifies a text/event-stream response
// content type drives the response recompute (SSE path) and yields a payload.
func TestComputeNormalized_ResponseStreamFlag(t *testing.T) {
	h := newNormalizeHandler()
	sse := []byte("data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" there\"}}]}\n\ndata: [DONE]\n\n")
	in := &trafficstore.NormalizeInput{
		Found:               true,
		AdapterType:         "openai",
		Model:               "gpt-4o-mini",
		Path:                "/v1/chat/completions",
		ResponseContentType: "text/event-stream",
		ResponseBody:        sse,
	}

	out := h.computeNormalized("evt-2", in)
	if len(out.ResponseNormalized) == 0 {
		t.Fatal("responseNormalized is empty for an SSE response")
	}
	if out.ResponseStatus == nil {
		t.Fatal("responseStatus must be set")
	}
}
