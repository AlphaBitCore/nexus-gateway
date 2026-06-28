package traffic

import (
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/store/trafficstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// TestComputeNormalized_FromCompressedColumnBody locks the only source of the
// normalized projection now that it is never persisted: the control plane
// recomputes it at view time from the stored raw body, and that body travels
// gateway → Hub → column as the compressed inline form (s2 is the default codec).
// A stale binary once produced a 404 on /traffic/:id/normalized here, so this test
// drives the full chain — compress on capture, wire round-trip, column payload,
// view-time decode — and asserts the recompute still yields a real ai-chat
// projection with no persisted sidecar row involved.
func TestComputeNormalized_FromCompressedColumnBody(t *testing.T) {
	// Force inline compression so NewInlineBody compresses the body regardless of
	// the production size floor; the codec is the process default (s2). Reset after.
	audit.SetInlineCompression(true, 1, 3)
	defer audit.SetInlineCompression(false, 0, 0)

	orig := []byte(`{"model":"mock-gpt-4o-mini","messages":[{"role":"user","content":"hello pii probe"}]}`)

	// Capture side: gateway builds the inline body and marshals it for the wire.
	captured := audit.NewInlineBody(orig, int64(len(orig)), false, "application/json")
	wire, err := json.Marshal(captured)
	if err != nil {
		t.Fatalf("marshal captured body: %v", err)
	}

	// Hub side: unmarshal the wire envelope, then render the persisted column
	// payload (a pure copy for a compressed body — base64 of the s2 frame).
	var received audit.Body
	if err := json.Unmarshal(wire, &received); err != nil {
		t.Fatalf("unmarshal wire body: %v", err)
	}
	payload, encoding := received.ColumnPayload()
	if encoding != audit.BodyColumnS2 {
		t.Fatalf("expected s2 column encoding (the default codec), got %q", encoding)
	}

	// View side: the control plane decodes the column back to the original
	// captured bytes and recomputes the normalized projection from them.
	decoded := audit.DecodeBodyForColumn(payload, encoding)
	if string(decoded) != string(orig) {
		t.Fatalf("compressed column round-trip mismatch:\n got  %q\n want %q", decoded, orig)
	}

	h := newNormalizeHandler()
	out := h.computeNormalized("evt-compressed", &trafficstore.NormalizeInput{
		Found:              true,
		AdapterType:        "openai",
		Model:              "mock-gpt-4o-mini",
		Path:               "/v1/chat/completions",
		RequestContentType: "application/json",
		RequestBody:        decoded,
	})

	if len(out.RequestNormalized) == 0 {
		t.Fatal("requestNormalized empty — view-time recompute from the compressed body produced nothing")
	}
	if out.RequestStatus == nil || *out.RequestStatus != "ok" {
		t.Fatalf("requestStatus = %v, want ok", out.RequestStatus)
	}
	if !strings.Contains(string(out.RequestNormalized), "ai-chat") {
		t.Fatalf("normalized payload is not an ai-chat projection: %s", out.RequestNormalized)
	}
	if !strings.Contains(string(out.RequestNormalized), "hello pii probe") {
		t.Fatalf("normalized payload missing the decoded prompt text: %s", out.RequestNormalized)
	}
}
