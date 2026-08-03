package proxy

// realtime_handler_test.go — the pre-upgrade admission chain: WebSocket
// preconditions, auth, rate limit, the inverted entitlement gate (AC-2), the
// OpenAI-format target filter, pricing/quota gates (AC-4's 503 arm), the
// static dial-failure 502 (AC-6), and the pure URL/header helpers.

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/realtimeproxy"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// doRealtimeHTTP drives ServeRealtime with a plain recorder — valid for
// every PRE-ACCEPT arm (each writes an ordinary HTTP error before any
// hijack).
func doRealtimeHTTP(deps *Deps, target string, upgrade bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if upgrade {
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Connection", "Upgrade")
	}
	rr := httptest.NewRecorder()
	NewHandler(deps).ServeRealtime(rtIngress())(rr, req)
	return rr
}

func assertErrCode(t *testing.T, rr *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rr.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, wantStatus, rr.Body.String())
	}
	if got := gjson.GetBytes(rr.Body.Bytes(), "error.code").String(); got != wantCode {
		t.Fatalf("error.code = %q, want %q; body=%s", got, wantCode, rr.Body.String())
	}
}

func TestServeRealtime_UpgradeRequired(t *testing.T) {
	deps, _ := realtimeDeps(t, "http://127.0.0.1:0")
	rr := doRealtimeHTTP(deps, "/v1/realtime?model=gpt-realtime", false)
	assertErrCode(t, rr, http.StatusBadRequest, "REALTIME_UPGRADE_REQUIRED")
}

func TestServeRealtime_ModelRequired(t *testing.T) {
	deps, _ := realtimeDeps(t, "http://127.0.0.1:0")
	rr := doRealtimeHTTP(deps, "/v1/realtime", true)
	assertErrCode(t, rr, http.StatusBadRequest, "REALTIME_MODEL_REQUIRED")
}

func TestServeRealtime_AuthErrorPreUpgrade(t *testing.T) {
	deps, _ := realtimeDeps(t, "http://127.0.0.1:0", func(d *Deps) {
		d.VKAuth = &rtVKAuth{authErr: vkauth.ErrMissing}
	})
	rr := doRealtimeHTTP(deps, "/v1/realtime?model=gpt-realtime", true)
	assertErrCode(t, rr, http.StatusUnauthorized, "AUTH_KEY_MISSING")
}

// denyAllLimiter rejects every Allow call with a retry hint.
type denyAllLimiter struct{}

func (denyAllLimiter) Allow(_ string, _ int, _ int64) (bool, int) { return false, 7 }

func TestServeRealtime_RateLimited(t *testing.T) {
	rpm := 1
	meta := entitledVKMeta("vk-rl")
	meta.RateLimitRpm = &rpm
	deps, _ := realtimeDeps(t, "http://127.0.0.1:0", func(d *Deps) {
		d.VKAuth = &rtVKAuth{meta: meta, hash: "h"}
		d.RateLimiter = denyAllLimiter{}
	})
	rr := doRealtimeHTTP(deps, "/v1/realtime?model=gpt-realtime", true)
	assertErrCode(t, rr, http.StatusTooManyRequests, "RATE_LIMITED")
	if rr.Header().Get("Retry-After") != "7" {
		t.Errorf("Retry-After = %q, want 7", rr.Header().Get("Retry-After"))
	}
}

// TestServeRealtime_EntitlementGate is AC-2's admission half: the INVERTED
// AllowedModels semantics. An EMPTY list (unrestricted everywhere else) is
// NOT entitled; a non-matching list is not entitled; every non-routable arm
// wears the SAME 404 envelope (non-disclosure); an entitled VK passes the
// gate (it then fails later at the dead-provider dial with 502 — proving the
// gate, not the dial, was the discriminator).
func TestServeRealtime_EntitlementGate(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Deps)
		want string // "404" or "pass"
	}{
		{"empty AllowedModels refused", func(d *Deps) {
			meta := entitledVKMeta("vk-e")
			meta.AllowedModels = nil
			d.VKAuth = &rtVKAuth{meta: meta, hash: "h"}
		}, "404"},
		{"non-matching list refused", func(d *Deps) {
			meta := entitledVKMeta("vk-n")
			meta.AllowedModels = []store.AllowedModelRef{{ProviderID: "p-openai", ModelID: "gpt-4o"}}
			d.VKAuth = &rtVKAuth{meta: meta, hash: "h"}
		}, "404"},
		{"unknown model fails closed", func(d *Deps) {
			d.Models = rtStubModels{model: nil}
		}, "404"},
		{"model resolve error fails closed", func(d *Deps) {
			d.Models = rtStubModels{model: rtPricedModel(), catalogErr: errors.New("catalog down")}
		}, "404"},
		{"no models dependency fails closed", func(d *Deps) {
			d.Models = nil
		}, "404"},
		{"entitled VK passes the gate", func(_ *Deps) {}, "pass"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, _ := realtimeDeps(t, "http://127.0.0.1:0", tc.mut)
			rr := doRealtimeHTTP(deps, "/v1/realtime?model=gpt-realtime", true)
			if tc.want == "404" {
				assertErrCode(t, rr, http.StatusNotFound, "NO_COMPATIBLE_PROVIDER")
				return
			}
			// Past the gate: the dead provider yields the dial 502, never 404.
			assertErrCode(t, rr, http.StatusBadGateway, "REALTIME_UPSTREAM_UNAVAILABLE")
		})
	}
}

// TestServeRealtime_NoSeamAuthenticatorStillGated covers the fallback branch
// for a VK authenticator WITHOUT the by-hash seam: plain Authenticate is
// used and the entitlement gate still applies.
func TestServeRealtime_NoSeamAuthenticatorStillGated(t *testing.T) {
	deps, _ := realtimeDeps(t, "http://127.0.0.1:0", func(d *Deps) {
		// stubVKAuthCacheTest implements only Authenticate; its meta has no
		// AllowedModels, so the dark gate refuses it.
		d.VKAuth = &stubVKAuthCacheTest{meta: &vkauth.VKMeta{ID: "vk-1", OrganizationID: "org-1"}}
	})
	rr := doRealtimeHTTP(deps, "/v1/realtime?model=gpt-realtime", true)
	assertErrCode(t, rr, http.StatusNotFound, "NO_COMPATIBLE_PROVIDER")
}

// TestServeRealtime_NonOpenAITargetsFiltered: routed targets that are not
// OpenAI-format are not relayable; zero survivors → the same 404 envelope.
func TestServeRealtime_NonOpenAITargetsFiltered(t *testing.T) {
	deps, _ := realtimeDeps(t, "http://127.0.0.1:0", func(d *Deps) {
		d.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
			ProviderID: "p-anthropic", ProviderName: "anthropic",
			ModelID: "m-rt", ProviderModelID: "claude-realtime", AdapterType: "anthropic",
		}}}
	})
	rr := doRealtimeHTTP(deps, "/v1/realtime?model=gpt-realtime", true)
	assertErrCode(t, rr, http.StatusNotFound, "NO_COMPATIBLE_PROVIDER")
}

func TestServeRealtime_NoRouteTargets(t *testing.T) {
	// No routing rule resolves targets AND the requested model is not in the
	// catalog, so the requested-model passthrough (FIX: parallel handlers gained
	// the same no-match passthrough as ServeProxy) also finds nothing → the
	// handler surfaces a 404 no-compatible-provider. (A resolvable model with no
	// rule would instead passthrough and dial — that is the passthrough path,
	// covered elsewhere.)
	deps, _ := realtimeDeps(t, "http://127.0.0.1:0", func(d *Deps) {
		d.Router = &stubRouterCacheTest{targets: nil}
		d.Models = rtStubModels{model: nil}
	})
	rr := doRealtimeHTTP(deps, "/v1/realtime?model=gpt-realtime", true)
	assertErrCode(t, rr, http.StatusNotFound, "NO_COMPATIBLE_PROVIDER")
}

// TestServeRealtime_UnpricedModel is AC-4's pricing-completeness arm plus
// AC-7's literal-zero clause: nil OR zero primary rates under an ENFORCED
// cost quota → 503 pre-upgrade; the same rows without enforcement pass the
// gate (dead provider → 502).
func TestServeRealtime_UnpricedModel(t *testing.T) {
	zeroAudio := rtPricedModel()
	zeroAudio.AudioInputPricePM = fPtr(0) // literal zero, non-nil

	nilAudio := rtPricedModel()
	nilAudio.AudioOutputPricePM = nil

	cases := []struct {
		name     string
		model    *store.Model
		enforced bool
		want     string
	}{
		{"literal-zero rate + enforced quota refused", zeroAudio, true, "503"},
		{"nil rate + enforced quota refused", nilAudio, true, "503"},
		{"nil rate without enforcement relays", nilAudio, false, "pass"},
		{"fully priced + enforced quota admits", rtPricedModel(), true, "pass"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The pricing-completeness gate keys off the PINNED target, so it
			// runs POST-dial (a failover may pin a different model). Use a live
			// provider so the dial succeeds and the gate can fire; the 503 arm
			// asserts the dialed upstream is then closed (no orphan spend).
			provider := newRTProvider(t, rtHoldScript)
			deps, _ := realtimeDeps(t, provider.srv.URL, func(d *Deps) {
				d.Models = rtStubModels{model: tc.model}
				if tc.enforced {
					d.QuotaEngine = newQuotaEngineWithPolicy(t, "virtual_key", "reject", 100_000, 0)
				}
			})
			if tc.want == "503" {
				rr := doRealtimeHTTP(deps, "/v1/realtime?model=gpt-realtime", true)
				assertErrCode(t, rr, http.StatusServiceUnavailable, "QUOTA_MODEL_UNPRICED")
				// The upstream dialed for the pinned target is closed rather
				// than left to accrue audio-token spend.
				provider.waitClosed(t, 5*time.Second)
				return
			}
			// The "pass" arms establish a live session; drive it over a real
			// WS client and close cleanly.
			srv, done := rtServer(t, deps)
			c, _, err := rtDial(t, srv.URL, "gpt-realtime", nil)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			_ = c.Close(websocket.StatusNormalClosure, "done")
			waitHandlerDone(t, done, 1)
		})
	}
}

// TestServeRealtime_QuotaStrictlyExceeded: the zero-estimate admission
// boundary — strictly over a reject cap refuses 429 pre-upgrade; EXACTLY at
// the limit admits (current + 0 > limit is false at equality; the
// per-response evaluation catches it after the first response — AC-8's
// documented boundary).
func TestServeRealtime_QuotaStrictlyExceeded(t *testing.T) {
	t.Run("strictly exceeded refused", func(t *testing.T) {
		deps, _ := realtimeDeps(t, "http://127.0.0.1:0", func(d *Deps) {
			d.QuotaEngine = newQuotaEngineWithPolicy(t, "virtual_key", "reject", 100, 500)
		})
		// The seeded usage rows are keyed on vk-1 (the shared harness id).
		meta := entitledVKMeta("vk-1")
		deps.VKAuth = &rtVKAuth{meta: meta, hash: "h"}
		rr := doRealtimeHTTP(deps, "/v1/realtime?model=gpt-realtime", true)
		assertErrCode(t, rr, http.StatusTooManyRequests, "QUOTA_EXCEEDED")
	})
	t.Run("exactly at limit admits", func(t *testing.T) {
		deps, _ := realtimeDeps(t, "http://127.0.0.1:0", func(d *Deps) {
			d.QuotaEngine = newQuotaEngineWithPolicy(t, "virtual_key", "reject", 100, 100)
		})
		meta := entitledVKMeta("vk-1")
		deps.VKAuth = &rtVKAuth{meta: meta, hash: "h"}
		rr := doRealtimeHTTP(deps, "/v1/realtime?model=gpt-realtime", true)
		assertErrCode(t, rr, http.StatusBadGateway, "REALTIME_UPSTREAM_UNAVAILABLE")
	})
}

// TestServeRealtime_DialFailure502Static is AC-6's dial-hygiene arm: every
// dial failure produces ONE static 502 body (no dial error text, no
// upstream host), and the dial path logs neither the provider key nor the
// dial URL.
func TestServeRealtime_DialFailure502Static(t *testing.T) {
	var logBuf bytes.Buffer
	deps, _ := realtimeDeps(t, "http://127.0.0.1:0", func(d *Deps) {
		d.Logger = slog.New(slog.NewTextHandler(&logBuf, nil))
	})
	rr := doRealtimeHTTP(deps, "/v1/realtime?model=gpt-realtime", true)
	assertErrCode(t, rr, http.StatusBadGateway, "REALTIME_UPSTREAM_UNAVAILABLE")

	body := rr.Body.String()
	for _, leak := range []string{"127.0.0.1", "ws://", "wss://", "dial", "connection refused"} {
		if strings.Contains(body, leak) {
			t.Errorf("502 body leaks dial detail %q: %s", leak, body)
		}
	}
	logs := logBuf.String()
	for _, leak := range []string{"sk-upstream", "ws://", "wss://", "Authorization"} {
		if strings.Contains(logs, leak) {
			t.Errorf("dial path logged %q: %s", leak, logs)
		}
	}
}

// TestServeRealtime_ResolverArms: a nil resolver and an always-failing
// resolver both end in the static 502 (resolution failures burn a dial
// attempt, never leak).
func TestServeRealtime_ResolverArms(t *testing.T) {
	t.Run("nil resolver", func(t *testing.T) {
		deps, _ := realtimeDeps(t, "http://127.0.0.1:0", func(d *Deps) { d.Resolver = nil })
		rr := doRealtimeHTTP(deps, "/v1/realtime?model=gpt-realtime", true)
		assertErrCode(t, rr, http.StatusBadGateway, "REALTIME_UPSTREAM_UNAVAILABLE")
	})
	t.Run("resolve error", func(t *testing.T) {
		deps, _ := realtimeDeps(t, "http://127.0.0.1:0", func(d *Deps) {
			d.Resolver = rtStubResolver{err: errors.New("no credential")}
		})
		rr := doRealtimeHTTP(deps, "/v1/realtime?model=gpt-realtime", true)
		assertErrCode(t, rr, http.StatusBadGateway, "REALTIME_UPSTREAM_UNAVAILABLE")
	})
}

// --- pure helpers ---------------------------------------------------------

func TestHeaderHasToken(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"websocket", true},
		{"WebSocket", true},
		{"h2c, websocket", true},
		{" websocket ", true},
		{"", false},
		{"h2c", false},
		{"websockets", false},
	}
	for _, tc := range cases {
		if got := headerHasToken(tc.value, "websocket"); got != tc.want {
			t.Errorf("headerHasToken(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

// TestRealtimeDialURL pins the derivation contract: scheme swap, /v1 path
// append below any base prefix, and — the security property — url.Values
// encoding so a hostile provider model id cannot inject into the dial URL.
func TestRealtimeDialURL(t *testing.T) {
	cases := []struct {
		name, base, model, want string
		wantErr                 bool
	}{
		{"https to wss", "https://api.openai.com", "gpt-realtime", "wss://api.openai.com/v1/realtime?model=gpt-realtime", false},
		{"http to ws (local provider)", "http://127.0.0.1:9999", "m", "ws://127.0.0.1:9999/v1/realtime?model=m", false},
		{"base path prefix preserved", "https://gw.example.com/openai/", "m", "wss://gw.example.com/openai/v1/realtime?model=m", false},
		{"ws base kept", "ws://h", "m", "ws://h/v1/realtime?model=m", false},
		{"injection-safe model id", "https://h", "m&stream=true#x", "wss://h/v1/realtime?model=m%26stream%3Dtrue%23x", false},
		{"unsupported scheme", "ftp://h", "m", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := realtimeDialURL(tc.base, tc.model)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("= (%q, %v), want %q", got, err, tc.want)
			}
		})
	}
}

func TestFilterRealtimeTargets(t *testing.T) {
	in := []routingcore.RoutingTarget{
		{ProviderID: "a", AdapterType: "anthropic"},
		{ProviderID: "b", AdapterType: "openai"},
		{ProviderID: "c", AdapterType: "OpenAI"}, // case-insensitive
		{ProviderID: "d", AdapterType: "gemini"},
	}
	out := filterRealtimeTargets(in)
	if len(out) != 2 || out[0].ProviderID != "b" || out[1].ProviderID != "c" {
		t.Fatalf("filtered = %+v, want [b c] preserving routed order", out)
	}
}

func TestStampRealtimeSession(t *testing.T) {
	md := stampRealtimeSession(nil, 1234, 3, realtimeproxy.ReasonQuotaExceeded)
	m, ok := md.(map[string]any)
	if !ok {
		t.Fatalf("metadata is %T, want map", md)
	}
	rt, _ := m["realtime"].(map[string]any)
	if rt["sessionDurationMs"] != int64(1234) || rt["responses"] != 3 || rt["closeReason"] != "REALTIME_QUOTA_EXCEEDED" {
		t.Fatalf("realtime metadata = %+v", rt)
	}
	// Normal close labels "normal", and pre-existing metadata is preserved.
	md2 := stampRealtimeSession(map[string]any{"cost": map[string]any{"unpriced": true}}, 5, 0, realtimeproxy.ReasonNormal)
	m2 := md2.(map[string]any)
	if m2["cost"] == nil {
		t.Error("pre-existing metadata dropped")
	}
	if m2["realtime"].(map[string]any)["closeReason"] != "normal" {
		t.Errorf("normal close label = %v", m2["realtime"].(map[string]any)["closeReason"])
	}
}
