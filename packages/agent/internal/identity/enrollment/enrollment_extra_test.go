package enrollment

import (
	"context"
	"strings"
	"testing"
)

// TestDeregister_BadBaseURLReturnsError covers the "create hub deregister
// request" error branch in Deregister: when the BaseURL contains an invalid
// character (e.g. a control character), http.NewRequestWithContext returns an
// error before any network call is made. This is distinct from a transport
// error (already tested) — it verifies the request-construction error arm.
func TestDeregister_BadBaseURLReturnsError(t *testing.T) {
	// A URL containing a control character is rejected by net/http's URL parser,
	// causing http.NewRequestWithContext to return an error.
	client := &HubEnrollClient{
		BaseURL:    "http://host\x00invalid",
		HTTPClient: nil, // never reached — error is at request construction
	}
	err := client.Deregister(context.Background(), "dev-tok", "thing-1", "test")
	if err == nil {
		t.Fatal("expected error on invalid BaseURL")
	}
	if !strings.Contains(err.Error(), "create hub deregister request") {
		t.Errorf("error should mention create hub deregister request: %v", err)
	}
}
