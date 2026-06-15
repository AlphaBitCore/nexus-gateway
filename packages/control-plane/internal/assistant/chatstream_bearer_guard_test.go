package assistant

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// TestChatStreamRejectsNonBearerPrincipal is the C1 guard: a request authenticated
// by x-admin-key (or any non-bearer principal) has no forwardable bearer, so the
// agent's admin self-calls would all 401 while still billing the system VK. It must
// be rejected (422) before any inference — never a 202 + zero-capability bill.
func TestChatStreamRejectsNonBearerPrincipal(t *testing.T) {
	h := New(Config{SystemVK: "nvk_test", Model: "m"})
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/assistant/sessions/s1/chat", strings.NewReader(`{"message":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Key", "some-api-key") // non-bearer principal: no Authorization bearer
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("s1")
	if err := h.StartChat(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a non-bearer principal must be rejected with 422, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "event:") {
		t.Fatal("rejection must be an HTTP error, not an SSE stream — no system VK should be billed")
	}
}
