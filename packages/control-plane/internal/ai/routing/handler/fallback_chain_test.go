package routing

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/labstack/echo/v4"
)

// TestValidateFallbackChain tables the write-boundary shape check. The AI
// Gateway resolver unmarshals fallbackChain into []core.FallbackChainEntry
// with a best-effort decode — a malformed chain silently yields ZERO recovery
// targets — so the validator must reject anything the resolver cannot parse
// into usable entries.
func TestValidateFallbackChain(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		wantOK bool
	}{
		{"nil", "", true},
		{"null", `null`, true},
		{"empty-array", `[]`, true},
		{"valid-single", `[{"providerId":"p1","modelId":"m1"}]`, true},
		{"valid-multi", `[{"providerId":"p1","modelId":"m1"},{"providerId":"p2","modelId":"m2"}]`, true},
		{"bare-strings", `["gpt-4o"]`, false},
		{"object-not-array", `{"providerId":"p","modelId":"m"}`, false},
		{"missing-modelId", `[{"providerId":"p1"}]`, false},
		{"empty-providerId", `[{"providerId":"","modelId":"m1"}]`, false},
		{"numbers", `[1,2]`, false},
		{"garbage", `not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.raw != "" {
				raw = json.RawMessage(tc.raw)
			}
			msg, ok := validateFallbackChain(raw)
			if ok != tc.wantOK {
				t.Fatalf("validateFallbackChain(%s) = (%q, %v); wantOK=%v", tc.raw, msg, ok, tc.wantOK)
			}
			if !ok && msg == "" {
				t.Fatalf("invalid input must carry a message")
			}
		})
	}
}

// TestCreateRoutingRule_RejectsBareStringFallbackChain is the regression test
// for the silent-failover-loss bug: an admin sending fallbackChain as bare
// model strings must get a 400 at create, not a rule whose recovery targets
// silently decode to nothing at the gateway.
func TestCreateRoutingRule_RejectsBareStringFallbackChain(t *testing.T) {
	h, _, _ := newAdminHandlerWithHubSpy(t)
	body := `{"name":"r","strategyType":"single","config":{},"fallbackChain":["gpt-4o"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/routing-rules", bytes.NewReader([]byte(body)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "Admin", "admin-1")

	if err := h.CreateRoutingRule(c); err != nil {
		t.Fatalf("CreateRoutingRule: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s; want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fallback_chain_invalid") {
		t.Errorf("missing fallback_chain_invalid code; body: %s", rec.Body.String())
	}
}
