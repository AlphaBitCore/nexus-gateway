package senders

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

// TestRegistry_RoundTrip exercises NewRegistry + Register + Get for the
// happy path and the not-found error branch — these three methods make
// the Registry pattern observable to the dispatcher; without coverage
// any future refactor that breaks the map lookup would slip past.
func TestRegistry_RoundTrip(t *testing.T) {
	r := NewRegistry()
	if r == nil {
		t.Fatal("NewRegistry returned nil")
	}

	// Get on empty registry returns wrapped error.
	if _, err := r.Get("slack"); err == nil {
		t.Error("Get on empty registry must error")
	} else if !strings.Contains(err.Error(), "slack") {
		t.Errorf("Get error should quote the channel type: %v", err)
	}

	// Register, then Get returns the same instance.
	s := stubSender{}
	r.Register("slack", &s)
	got, err := r.Get("slack")
	if err != nil {
		t.Fatalf("Get after Register: %v", err)
	}
	if got != &s {
		t.Error("Get returned a different Sender than Register installed")
	}
}

// TestPostJSON_NewRequestError covers the
// `http.NewRequestWithContext` error branch — a URL containing
// control characters fails before any I/O. Without this the
// pre-Do error path stays uncovered.
func TestPostJSON_NewRequestError(t *testing.T) {
	_, err := postJSON(context.Background(), http.DefaultClient, "http://\x7f", []byte("{}"), nil)
	if err == nil {
		t.Fatal("postJSON should fail when URL is unparseable")
	}
}

// TestPostJSON_AppliesCustomHeaders covers the header-copy loop in
// postJSON — without this the `for k, vs := range headers` branch
// stays uncovered and a future bug that swallows custom headers
// would not be caught.
func TestPostJSON_AppliesCustomHeaders(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	h := http.Header{}
	h.Set("Authorization", "Bearer abc")
	status, err := postJSON(context.Background(), http.DefaultClient, srv.URL, []byte("{}"), h)
	if err != nil {
		t.Fatalf("postJSON: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status: got %d", status)
	}
	if gotAuth != "Bearer abc" {
		t.Errorf("custom Authorization header dropped: got %q", gotAuth)
	}
}

// stubSender is a minimal Sender that returns a fixed status — used to
// confirm Registry.Get returns the exact instance Registered.
type stubSender struct{}

func (s *stubSender) Send(_ context.Context, _ alerting.Channel, _ alerting.Alert) (int, error) {
	return 200, nil
}
