package normalize

import (
	"context"
	"strings"
	"testing"

	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Lives here rather than in core because it needs the real codec registry, and codecs
// imports core.

// TestNormalize_WellFormedBodyStillNormalizes is the no-false-positive assertion. The guard
// is at the single entry every tier flows through, so a bug in it would silently disable
// normalization for real traffic — the audit row would keep writing, just without structured
// content, which is exactly how finding C-17 hid.
func TestNormalize_WellFormedBodyStillNormalizes(t *testing.T) {
	reg := BuildRegistry()
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"path C:\\tmp and \"quoted\""}]}`)

	payload, err := reg.Normalize(context.Background(), body, normcore.Meta{
		AdapterType: "openai", Direction: normcore.DirectionRequest, ContentType: "application/json",
		EndpointPath: "/v1/chat/completions",
	})
	if err != nil {
		t.Fatalf("Normalize on a well-formed body with escapes: %v — the guard must not reject "+
			"real traffic; escaped quotes and backslashes are ordinary in chat content", err)
	}
	if strings.TrimSpace(string(payload.Kind)) == "" {
		t.Fatalf("payload Kind is empty: %+v", payload)
	}
}
