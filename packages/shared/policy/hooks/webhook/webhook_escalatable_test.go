package webhook

import (
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// TestWebhookForward_MayExceedOnMatch asserts webhook-forward satisfies
// core.RuntimeEscalatable and reports true, so the streaming-routing predicates
// (Pipeline.MayBlock / MayRedact) over-route it to buffer regardless of its
// declared onMatch ceiling. Without this, a webhook configured with an approve
// ceiling whose remote reply returns reject_hard / modify would be left on the
// audit-only live path and have its enforcement silently dropped (HIGH-1).
func TestWebhookForward_MayExceedOnMatch(t *testing.T) {
	cfg := &core.HookConfig{
		ID:               "wh-1",
		ImplementationID: "webhook-forward",
		Name:             "test-webhook",
		// No onMatch block → factory sets the advisory approve ceiling, the exact
		// configuration HIGH-1 leaks under.
		Config: map[string]any{"endpoint": "http://example.com"},
	}
	h, err := NewWebhookForwardWithClient(cfg, http.DefaultClient)
	if err != nil {
		t.Fatalf("NewWebhookForwardWithClient: %v", err)
	}

	esc, ok := h.(core.RuntimeEscalatable)
	if !ok {
		t.Fatal("webhook-forward does not satisfy core.RuntimeEscalatable; predicates cannot detect its runtime escalation")
	}
	if !esc.MayExceedOnMatch() {
		t.Error("MayExceedOnMatch()=false; a webhook with an approve ceiling would leak its runtime block/redact onto the live path")
	}
}
