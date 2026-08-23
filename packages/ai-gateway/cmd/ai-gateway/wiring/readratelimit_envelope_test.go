package wiring

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
)

type denyLimiter struct{}

func (denyLimiter) Allow(string, int, int64) (bool, int) { return false, 7 }

type fixedVK struct{ rpm int }

func (f fixedVK) Authenticate(context.Context, *http.Request) (*vkauth.VKMeta, error) {
	return &vkauth.VKMeta{ID: "vk-1", RateLimitRpm: &f.rpm}, nil
}

// A 429 from a read route is the same 429 a data-plane route gives.
//
// This wrapper answered with http.Error — text/plain "rate limit exceeded",
// with no code and no type — while the proxy path answered a JSON envelope
// carrying RATE_LIMITED and a hint. A client backing off on the gateway's
// rate limit had to parse two different things depending on which route it
// hit, and on this one there was nothing to parse.
func TestVKReadRateLimit_RejectsWithTheGatewayErrorEnvelope(t *testing.T) {
	h := vkReadRateLimit(fixedVK{rpm: 60}, denyLimiter{}, discardLogger())(
		func(w http.ResponseWriter, r *http.Request) { t.Error("the inner handler ran despite the limit") })

	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if got := rr.Header().Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After = %q, want the limiter's own 7", got)
	}
	if got := rr.Header().Get("X-RateLimit-Limit"); got != "60" {
		t.Errorf("X-RateLimit-Limit = %q, want 60", got)
	}
	var env struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rr.Body)
	}
	if got, _ := env.Error["code"].(string); got != "RATE_LIMITED" {
		t.Errorf("error.code = %v, want RATE_LIMITED — the same code the data-plane path emits", env.Error["code"])
	}
	if got, _ := env.Error["type"].(string); got != "rate_limit_error" {
		t.Errorf("error.type = %v, want rate_limit_error", env.Error["type"])
	}
}
