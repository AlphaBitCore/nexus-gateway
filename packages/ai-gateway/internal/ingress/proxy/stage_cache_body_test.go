// stage_cache_body_test.go — the output-ceiling wiring the cache stage
// owes the codec.
//
// These tests drive the WHOLE chain (router → cache stage →
// prepareUpstreamBody → CallTarget → codec → wire) and assert on the bytes
// a capturing upstream actually received. That is deliberate: the codec's
// own tests inject a CallTarget with the ceiling already set, so they pass
// whether or not anything populates it. A live stack disagreed with them —
// RoutingTarget did not carry the ceiling, the CallTarget hand-built here
// was therefore built with 0, and both codec branches misfired on the only
// attempt most requests ever make (the cache stage prepares targets[0]'s
// first attempt; the executor re-resolves a full target only on retry or
// failover).
package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// anthropicCaptureUpstream is an Anthropic-wire upstream that records the
// request body it was sent and replies with a minimal valid Messages
// response.
func anthropicCaptureUpstream(t *testing.T) (*httptest.Server, func() []byte) {
	t.Helper()
	var mu sync.Mutex
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = b
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",
			"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":3,"output_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}

// serveToAnthropicTarget routes an OpenAI-shape request to an
// anthropic-adapter target carrying maxOutputTokens, and returns the
// recorder plus the bytes the upstream received.
func serveToAnthropicTarget(t *testing.T, upstreamURL string, maxOutputTokens int, body string) *httptest.ResponseRecorder {
	t.Helper()
	cacheOpt, cleanup := withCache(t)
	t.Cleanup(cleanup)
	deps := makeOpenAIDeps(t, upstreamURL, emptyHookCache(t), cacheOpt, func(d *Deps) {
		d.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
			ProviderID:      "p-anthropic",
			ProviderName:    "anthropic",
			ProviderModelID: "claude-x",
			ModelID:         "claude-x",
			ModelName:       "Claude X",
			ModelCode:       "claude-x",
			AdapterType:     "anthropic",
			MaxOutputTokens: maxOutputTokens,
		}}}
	})
	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	return w
}

// A caller that echoes back an over-ceiling max_tokens must be clamped to
// the catalog ceiling on the wire. This is the exact production 400: the
// catalog advertised 131072 on /v1/models, a well-behaved client read it
// and sent it back, and Anthropic rejected `131072 > 128000`.
func TestServeProxy_AnthropicTarget_OverCeilingMaxTokens_ClampedOnTheWire(t *testing.T) {
	upstream, upstreamBody := anthropicCaptureUpstream(t)

	w := serveToAnthropicTarget(t, upstream.URL, 128000,
		`{"model":"claude-x","max_tokens":131072,"messages":[{"role":"user","content":"hello"}]}`)

	if got := gjson.GetBytes(upstreamBody(), "max_tokens").Int(); got != 128000 {
		t.Errorf("upstream max_tokens = %d, want it clamped to the catalog ceiling 128000; body=%s",
			got, upstreamBody())
	}
	if got := w.Header().Get("X-Nexus-Coerced"); !strings.Contains(got, "max_tokens→128000_model_max") {
		t.Errorf("X-Nexus-Coerced = %q, want it to record the clamp so the caller can see the applied cap", got)
	}
}

// A caller that omits max_tokens (legal on the OpenAI shape it came from)
// must get the model's real ceiling, not the codec's fallback floor.
// Anthropic requires the field, so the codec always synthesizes one; with
// the ceiling unwired it silently filled 8192 and truncated every long
// completion — an error-free failure the clamp test above cannot see.
func TestServeProxy_AnthropicTarget_OmittedMaxTokens_FilledWithModelCeiling(t *testing.T) {
	upstream, upstreamBody := anthropicCaptureUpstream(t)

	w := serveToAnthropicTarget(t, upstream.URL, 128000,
		`{"model":"claude-x","messages":[{"role":"user","content":"hello"}]}`)

	if got := gjson.GetBytes(upstreamBody(), "max_tokens").Int(); got != 128000 {
		t.Errorf("upstream max_tokens = %d, want the model ceiling 128000 (8192 = the fallback floor, i.e. the ceiling never reached the codec); body=%s",
			got, upstreamBody())
	}
	if got := w.Header().Get("X-Nexus-Coerced"); !strings.Contains(got, "max_tokens→128000_model_default") {
		t.Errorf("X-Nexus-Coerced = %q, want the synthesized cap recorded", got)
	}
}

// An under-ceiling caller is passed through untouched: the clamp must not
// rewrite a value the model accepts, and must not stamp a coercion the
// caller would have to explain.
func TestServeProxy_AnthropicTarget_UnderCeilingMaxTokens_PassedThroughUnchanged(t *testing.T) {
	upstream, upstreamBody := anthropicCaptureUpstream(t)

	w := serveToAnthropicTarget(t, upstream.URL, 128000,
		`{"model":"claude-x","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`)

	if got := gjson.GetBytes(upstreamBody(), "max_tokens").Int(); got != 1024 {
		t.Errorf("upstream max_tokens = %d, want the caller's 1024 preserved; body=%s", got, upstreamBody())
	}
	if got := w.Header().Get("X-Nexus-Coerced"); strings.Contains(got, "max_tokens") {
		t.Errorf("X-Nexus-Coerced = %q, want no max_tokens coercion for an in-range caller", got)
	}
}

// A catalog row with no maxOutputTokens (NULL column → 0) must still
// produce a request Anthropic accepts: no clamp, and the fallback floor
// filled when the caller omits the field. This is the branch that made the
// unwired path fail silently rather than loudly.
func TestServeProxy_AnthropicTarget_NoCatalogCeiling_UsesFallbackFloor(t *testing.T) {
	upstream, upstreamBody := anthropicCaptureUpstream(t)

	serveToAnthropicTarget(t, upstream.URL, 0,
		`{"model":"claude-x","messages":[{"role":"user","content":"hello"}]}`)

	if got := gjson.GetBytes(upstreamBody(), "max_tokens").Int(); got != 8192 {
		t.Errorf("upstream max_tokens = %d, want the 8192 fallback floor when the catalog has no ceiling; body=%s",
			got, upstreamBody())
	}
}

// With no catalog ceiling the clamp has nothing to enforce, so an
// over-ceiling caller must reach the wire unchanged rather than being
// clamped to 0 — a 0 would be rejected by Anthropic and would turn a
// missing catalog value into a hard failure for every caller.
func TestServeProxy_AnthropicTarget_NoCatalogCeiling_CallerValuePreserved(t *testing.T) {
	upstream, upstreamBody := anthropicCaptureUpstream(t)

	serveToAnthropicTarget(t, upstream.URL, 0,
		`{"model":"claude-x","max_tokens":131072,"messages":[{"role":"user","content":"hello"}]}`)

	if got := gjson.GetBytes(upstreamBody(), "max_tokens").Int(); got != 131072 {
		t.Errorf("upstream max_tokens = %d, want the caller's value preserved when no ceiling is known; body=%s",
			got, upstreamBody())
	}
}

// TestBodyPrepCallTarget_WireFieldsInert mechanizes the contract the cache
// stage's hand-built CallTarget depends on: PrepareBody must read ONLY the
// body-shaping fields bodyPrepCallTarget carries. The executor rebuilds a
// fuller CallTarget from provtarget.PgResolver on retry/failover — same
// body-shaping fields plus wire/dispatch fields (APIKey, credential identity,
// Extras). ServesResponsesAPI is NOT in the inert set: the native-leg triage
// reads it for /v1/responses bodies, so bodyPrepCallTarget carries it and
// both legs see the same value. If a codec ever starts reading an omitted
// field,
// the cache stage's first-attempt body diverges from the re-prepared body:
// the cache key breaks and attempt 1 differs from failover, silently. This
// test drives the REAL anthropic adapter with a lean target and a full target
// and asserts identical output — so that divergence becomes a red test the
// day it is introduced, not a production incident.
func TestBodyPrepCallTarget_WireFieldsInert(t *testing.T) {
	provReg := provcore.NewRegistry()
	provbuiltins.Register(provReg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	adapter, ok := provReg.Get(provcore.FormatAnthropic)
	if !ok {
		t.Fatal("anthropic adapter not registered")
	}

	lean := bodyPrepCallTarget(routingcore.RoutingTarget{
		ProviderID:      "p-anthropic",
		ProviderName:    "anthropic",
		AdapterType:     "anthropic",
		ProviderModelID: "claude-opus-4-7",
		BaseURL:         "https://api.anthropic.com",
		MaxOutputTokens: 128000,
	})

	// The executor's resolved target for the same model on a retry: identical
	// body-shaping fields, plus the wire/dispatch fields the cache stage omits.
	full := lean
	full.APIKey = "sk-secret"
	full.CredentialID = "cred-1"
	full.CredentialName = "primary"
	full.Extras = map[string]string{"azure.deployment": "prod", "anything": "x"}

	// Canonical OpenAI body translated to the Anthropic wire — the exact shape
	// the cache stage hands PrepareBody after canonicalization (BodyFormat
	// stays OpenAI; WireShape is the target's). This is the path where the
	// codec's clamp/fill actually runs; a native Anthropic-shape body would
	// pass through untranslated and never exercise the ceiling logic.
	body := []byte(`{"model":"claude-opus-4-7","max_tokens":131072,"messages":[{"role":"user","content":"hi"}]}`)
	req := provcore.Request{
		WireShape:  typology.WireShapeAnthropicMessages,
		BodyFormat: provcore.FormatOpenAI,
		Body:       body,
	}

	req.Target = lean
	leanBytes, leanRw, _, leanErr := adapter.PrepareBody(req)
	req.Target = full
	fullBytes, fullRw, _, fullErr := adapter.PrepareBody(req)

	if leanErr != nil || fullErr != nil {
		t.Fatalf("PrepareBody errors: lean=%v full=%v", leanErr, fullErr)
	}
	// Guard against a vacuous comparison: the codec must actually have shaped
	// the body (the ceiling clamp fired 131072→128000), so we are comparing
	// real transformed output, not two identical passthroughs.
	if !bytes.Contains(leanBytes, []byte(`"max_tokens":128000`)) {
		t.Fatalf("expected the ceiling clamp to fire; body=%s", leanBytes)
	}
	if !bytes.Equal(leanBytes, fullBytes) {
		t.Errorf("wire fields changed the prepared body — a codec is reading a field bodyPrepCallTarget omits:\n lean=%s\n full=%s", leanBytes, fullBytes)
	}
	if strings.Join(leanRw, ",") != strings.Join(fullRw, ",") {
		t.Errorf("wire fields changed the rewrites: lean=%v full=%v", leanRw, fullRw)
	}
}
