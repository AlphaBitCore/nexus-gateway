package wiring

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every miss answers in JSON, whatever the path looks like.
//
// Go's ServeMux answers an unmatched pattern with `404 page not found` as
// text/plain. The gateway registered a catch-all for /v1/ only, so the OpenAI
// surface got a parseable envelope while every other prefix — /v1beta, the
// Gemini surface's own root, a bare typo at the root — still fell through to
// the text/plain default. Both OpenAI SDKs and the Gemini client JSON-parse
// error bodies, so those misses surfaced as a status with no message at all.
func TestMountCoreRoutes_EveryMissAnswersWithAJSONEnvelope(t *testing.T) {
	h := getSharedCoreHandler(t)

	for _, path := range []string{
		"/v1/nope",
		"/v1beta/nope",
		"/v1beta/models/gemini-2.5-flash:noSuchAction",
		"/nope",
		"/openai/deployments/x/nope",
	} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))

			if rr.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rr.Code)
			}
			if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json — a text/plain body reaches an SDK as a status with no message", ct)
			}
			var env struct {
				Error map[string]any `json:"error"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil || env.Error == nil {
				t.Fatalf("body is not an error envelope: %s", rr.Body.String())
			}
			if s, _ := env.Error["message"].(string); !strings.Contains(s, path) {
				t.Errorf("error.message = %q, want it to name the path so the caller can tell which call was wrong", s)
			}
		})
	}
}

// A path the gateway serves under another method is a 405, not a 404.
//
// Go's ServeMux reaches its own 405/Allow branch only when NO pattern matched.
// Registering a catch-all at "/" matches everything, so that branch went dead
// for the whole surface and every wrong-method request became a 404 whose body
// said "this gateway does not serve /healthz" — about a path it does serve.
// The fallback now asks the mux which methods would have matched and answers
// 405 with Allow when any would.
func TestMountCoreRoutes_AWrongMethodIsA405NotA404(t *testing.T) {
	h := getSharedCoreHandler(t)

	for _, tc := range []struct{ method, path, wantAllow string }{
		{http.MethodPost, "/healthz", "GET"},
		{http.MethodGet, "/v1beta/models/gemini-2.5-flash:generateContent", "POST"},
		{http.MethodGet, "/openai/deployments/x/chat/completions", "POST"},
		{http.MethodPost, "/v1/models", "GET"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}")))

			if rr.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405 — the path is served, just not by this method (body %s)", rr.Code, rr.Body)
			}
			if got := rr.Header().Get("Allow"); !strings.Contains(got, tc.wantAllow) {
				t.Errorf("Allow = %q, want it to name %s", got, tc.wantAllow)
			}
			var env struct {
				Error map[string]any `json:"error"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil || env.Error == nil {
				t.Fatalf("body is not an error envelope: %s", rr.Body)
			}
			if s, _ := env.Error["message"].(string); !strings.Contains(s, tc.method) {
				t.Errorf("error.message = %q, want it to name the method that was refused", s)
			}
		})
	}
}

// /v1/audio/translations is deliberately unmounted. Only whisper-1 implements
// it upstream, and measurement showed every other speech model answering the
// provider's own "Invalid URL (POST /v1/audio/translations)" 404 — the wrong
// envelope to hand a caller, and about a route this deployment has no use for.
// Unmounted it falls to the gateway's own fallback, which at least says whose
// refusal it is.
func TestMountCoreRoutes_TranslationsIsNotServed(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/translations", strings.NewReader("x"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
	getSharedCoreHandler(t).ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rr.Code, rr.Body)
	}
	var env struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil || env.Error == nil {
		t.Fatalf("body is not the gateway's own envelope: %s", rr.Body)
	}
	if got, _ := env.Error["code"].(string); got != "ENDPOINT_NOT_SUPPORTED" {
		t.Errorf("error.code = %v, want ENDPOINT_NOT_SUPPORTED — an unmounted path must read as ours, not as the upstream's", env.Error["code"])
	}
}
