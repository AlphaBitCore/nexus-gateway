package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
)

// Every gateway-generated error envelope has to satisfy the same three rules,
// whichever route produced it:
//
//   - error.code is a STRING or absent. A number there is not the OpenAI shape
//     the gateway claims to speak — the SDKs type error.code as an optional
//     string — and it tells a caller nothing the HTTP status line has not
//     already said.
//   - error.type comes from OpenAI's vocabulary.
//   - error.message is non-empty, because a caller reading only the message
//     must be able to tell what was refused.
//
// The rules are asserted on the shared body builder rather than per route, so
// a route added later inherits them by construction instead of by review.
func assertGatewayErrorShape(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var env struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("not JSON: %v (%s)", err, body)
	}
	if env.Error == nil {
		t.Fatalf("no error object: %s", body)
	}
	if code, ok := env.Error["code"]; ok {
		if _, isString := code.(string); !isString {
			t.Errorf("error.code is %T (%v), want a string — a numeric code repeats the status line and breaks SDKs that type it as a string: %s", code, code, body)
		}
	}
	if s, _ := env.Error["type"].(string); s == "" {
		t.Errorf("error.type missing or not a string: %s", body)
	}
	if s, _ := env.Error["message"].(string); s == "" {
		t.Errorf("error.message missing or empty: %s", body)
	}
	return env.Error
}

func TestOpenAIProxyErrorBody_NeverEmitsANumericCode(t *testing.T) {
	for _, tc := range []struct {
		name           string
		status         int
		code, hint     string
		wantCodeAbsent bool
	}{
		{name: "named code", status: 400, code: "MODEL_REQUIRED"},
		{name: "named code with hint", status: 401, code: "AUTH_KEY_MISSING", hint: "send a virtual key"},
		{name: "no code at all", status: 500, wantCodeAbsent: true},
		{name: "no code on a 502", status: 502, wantCodeAbsent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := assertGatewayErrorShape(t, openAIProxyErrorBody(tc.status, tc.code, "something was refused", tc.hint))
			if tc.wantCodeAbsent {
				if _, present := inner["code"]; present {
					t.Errorf("no code was supplied, so error.code must be absent rather than filled with the status: %v", inner["code"])
				}
				return
			}
			if inner["code"] != tc.code {
				t.Errorf("error.code = %v, want %q", inner["code"], tc.code)
			}
		})
	}
}

// writeError names a machine code for the traffic row on every call. Withholding
// that same code from the caller leaves them a 400 whose only identity is the
// English sentence, while the gateway knew the answer all along.
func TestWriteError_GivesTheCallerTheCodeItRecords(t *testing.T) {
	for _, code := range []string{"QUOTA_EXCEEDED", "HOOK_BLOCKED", "REDACT_FAIL_CLOSED"} {
		rec, w := &audit.Record{}, httptest.NewRecorder()
		(&Handler{}).writeError(w, rec, 400, code, "refused")
		inner := assertGatewayErrorShape(t, w.Body.Bytes())
		if inner["code"] != code {
			t.Errorf("error.code = %v, want %q — the code is recorded on the traffic row but withheld from the caller", inner["code"], code)
		}
		if rec.ErrorCode != code {
			t.Errorf("rec.ErrorCode = %q, want %q", rec.ErrorCode, code)
		}
	}
}

// The 429 the data plane gives must say what the limit is. checkRateLimit set
// X-RateLimit-Limit only on the allow path — its caller stamps it after the
// reject has already returned — so the one response that most needs the number
// was the only one without it.
func TestCheckRateLimit_TheRejectionCarriesBothRateLimitHeaders(t *testing.T) {
	rpm := 60
	h := &Handler{deps: &Deps{RateLimiter: denyingLimiter{}}}
	w := httptest.NewRecorder()

	if err := h.checkRateLimit(w, &vkauth.VKMeta{ID: "vk-1", RateLimitRpm: &rpm}); err == nil {
		t.Fatal("the limiter denied but checkRateLimit allowed")
	}
	if got := w.Header().Get("X-RateLimit-Limit"); got != "60" {
		t.Errorf("X-RateLimit-Limit = %q, want 60 — a client cannot back off to a limit it is not told", got)
	}
	if got := w.Header().Get("Retry-After"); got != "9" {
		t.Errorf("Retry-After = %q, want the limiter's own 9", got)
	}
}

type denyingLimiter struct{}

func (denyingLimiter) Allow(string, int, int64) (bool, int) { return false, 9 }

// The compare endpoint has its own bucket with its own ceiling, and said
// nothing about it. checkRateLimit was fixed to stamp X-RateLimit-Limit before
// the limiter's verdict; this one still answered Retry-After alone, so a caller
// backing off on /v1/estimate was told when to retry and never what the limit
// is. "The 429s differ by route family" was the defect — this was a route
// family still differing.
func TestCheckCompareRateLimit_SaysWhatItsLimitIs(t *testing.T) {
	rpm := 7
	for _, tc := range []struct {
		name string
		meta *vkauth.VKMeta
		want string
	}{
		{"the key's own compare ceiling", &vkauth.VKMeta{ID: "vk-1", CompareEndpointRateLimitRpm: &rpm}, "7"},
		{"the default when the key sets none", &vkauth.VKMeta{ID: "vk-2"}, "30"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{deps: &Deps{RateLimiter: denyingLimiter{}}}
			w := httptest.NewRecorder()
			if err := h.checkCompareRateLimit(w, tc.meta); err == nil {
				t.Fatal("the limiter denied but checkCompareRateLimit allowed")
			}
			if got := w.Header().Get("X-RateLimit-Limit"); got != tc.want {
				t.Errorf("X-RateLimit-Limit = %q, want %q — a client cannot back off to a ceiling it is never told", got, tc.want)
			}
			if got := w.Header().Get("Retry-After"); got != "9" {
				t.Errorf("Retry-After = %q, want the limiter's own 9", got)
			}
		})
	}
}
