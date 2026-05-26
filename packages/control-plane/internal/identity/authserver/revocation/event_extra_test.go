package revocation_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
)

// TestEvent_UnmarshalJSON_MalformedReturnsError covers the json.Unmarshal
// branch in Event.UnmarshalJSON (line 60) — malformed wire data must be
// rejected before scope validation runs so callers can distinguish
// "wire corruption" from "unsupported scope".
func TestEvent_UnmarshalJSON_MalformedReturnsError(t *testing.T) {
	var ev revocation.Event
	err := json.Unmarshal([]byte(`{not valid json`), &ev)
	if err == nil {
		t.Fatal("expected unmarshal err on malformed payload")
	}
	if strings.Contains(err.Error(), "unknown scope") {
		t.Fatalf("malformed json must NOT surface as unknown-scope: %v", err)
	}
}

// TestEvent_UnmarshalJSON_WrongTypeReturnsError — when a typed field is
// present but holds a wire-incompatible type (scope as number), the
// json.Unmarshal step in UnmarshalJSON must fail before the scope guard
// runs.
func TestEvent_UnmarshalJSON_WrongTypeReturnsError(t *testing.T) {
	var ev revocation.Event
	err := json.Unmarshal([]byte(`{"scope":42}`), &ev)
	if err == nil {
		t.Fatal("expected type err on scope:number")
	}
}
