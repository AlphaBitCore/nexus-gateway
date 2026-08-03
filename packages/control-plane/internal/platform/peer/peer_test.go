package peer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/peerurl"
)

func TestStatic_AlwaysYieldsValue(t *testing.T) {
	p := Static("http://gw.internal:3050")
	got, err := p(context.Background())
	if err != nil || got != "http://gw.internal:3050" {
		t.Fatalf("Static = %q, %v; want fixed value, nil", got, err)
	}
}

// TestFromResolver_ResolvesPrivateURLFromHub drives the provider through the
// real shared resolver against a fake Hub: the provider must hit the
// service-url endpoint for its bound thing type with the service token and
// yield the Hub-reported privateUrl.
func TestFromResolver_ResolvesPrivateURLFromHub(t *testing.T) {
	var gotPath, gotAuth string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"thingType":"ai-gateway","privateUrl":"http://10.0.0.5:3050","publicUrl":"https://gw.example"}`))
	}))
	defer hub.Close()

	p := FromResolver(peerurl.New(hub.URL, "tok-internal"), "ai-gateway")
	got, err := p(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "http://10.0.0.5:3050" {
		t.Errorf("resolved %q, want the Hub-reported privateUrl", got)
	}
	if gotPath != "/api/internal/things/service-url/ai-gateway" {
		t.Errorf("hub path = %q", gotPath)
	}
	if gotAuth != "Bearer tok-internal" {
		t.Errorf("auth = %q, want the internal service token", gotAuth)
	}
}

// TestFromResolver_NotReportedYieldsError locks the no-silent-fallback
// contract: a peer that has not reported yet is an error, never a default.
func TestFromResolver_NotReportedYieldsError(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer hub.Close()

	p := FromResolver(peerurl.New(hub.URL, "tok"), "compliance-proxy")
	got, err := p(context.Background())
	if err == nil || got != "" {
		t.Fatalf("want error for unreported peer, got %q, %v", got, err)
	}
	if !errors.Is(err, peerurl.ErrNotReported) {
		t.Errorf("error must wrap peerurl.ErrNotReported, got %v", err)
	}
}

// TestServiceUnavailable_Writes503Envelope locks the failure UX binding:
// resolution failure is 503 with the canonical envelope and the
// machine-readable PEER_SERVICE_UNAVAILABLE code — never a raw 500.
func TestServiceUnavailable_Writes503Envelope(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)

	if err := ServiceUnavailable(c, "ai-gateway", ErrUnavailable); err != nil {
		t.Fatalf("ServiceUnavailable: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not the canonical envelope: %v (%s)", err, rec.Body.String())
	}
	if body.Error.Code != CodeUnavailable {
		t.Errorf("code = %q, want %q", body.Error.Code, CodeUnavailable)
	}
	if !strings.Contains(body.Error.Message, "ai-gateway not yet available") {
		t.Errorf("message must be human-readable, got %q", body.Error.Message)
	}
	if body.Error.Type != "peer_unavailable" {
		t.Errorf("type = %q", body.Error.Type)
	}
}
