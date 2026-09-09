package proxy

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	trafficanthropic "github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/anthropic"
	trafficopenai "github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

var anthropicIngress = Ingress{
	WireShape:  typology.WireShapeAnthropicMessages,
	BodyFormat: provcore.FormatAnthropic,
}

// canonicalLaneHandler wires the real bridge and the real normalize registry, so
// the request stage takes the canonical lane rather than the format-aware
// fallback.
func canonicalLaneHandler(t *testing.T) *Handler {
	t.Helper()
	return &Handler{deps: &Deps{
		HookConfigCache:   newPiiRedactHookCache(t),
		TrafficAdapter:    &trafficopenai.Adapter{},
		CanonicalBridge:   canonicalbridge.New(provbuiltins.SchemaCodecs(nil)),
		NormalizeRegistry: canonicalRegistry(),
		Logger:            slog.Default(),
	}}
}

// A redaction on a non-OpenAI ingress must EDIT the caller's bytes, never
// rebuild them — and this is the test that would have caught it when it did.
//
// The canonical lane briefly covered every ingress, which looked like a straight
// win: it made channels the flat extraction cannot represent scannable on
// Anthropic too. Its write-back encoded canonical back to the Anthropic wire,
// and that REBUILDS the request. Canonical chat does not model `cache_control`
// (so prompt caching stops working and the customer's Anthropic bill rises on
// exactly the requests a policy touched — an invariant this repo already pins
// elsewhere after a live incident), nor `metadata.user_id`, `mcp_servers`, or
// `content[].citations`. Worse, a server tool comes back as a client-side custom
// tool with no `type`, and the upstream then emits a tool_use and blocks on a
// tool_result the client cannot produce. That corrupts a conversation silently.
//
// So this ingress keeps the format-aware extraction and its surgical sjson
// write-back. The assertion is not "the text was masked" — it is "everything
// else is still there", because a rebuild masks the text correctly too.
func TestNonOpenAIIngressRedactionEditsTheRequestInsteadOfRebuildingIt(t *testing.T) {
	h := canonicalLaneHandler(t)
	// The ingress adapter has to be the one for this wire. Production resolves it
	// from the registry; this handler has none, so the single-adapter escape
	// hatch is set explicitly — handing an Anthropic body to the OpenAI adapter
	// yields no content and the assertions below would pass on an empty scan.
	h.deps.TrafficAdapter = &trafficanthropic.Adapter{}

	// A realistic Anthropic request: a cache breakpoint, end-user attribution,
	// and a SERVER tool — none of which canonical chat models.
	body := []byte(`{"model":"claude-3-5-sonnet-20241022","max_tokens":100,` +
		`"metadata":{"user_id":"u-42"},` +
		`"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":3}],` +
		`"system":[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text",` +
		`"text":"write to alice@example.com","cache_control":{"type":"ephemeral"}}]}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	auditRec := &audit.Record{RequestID: "req-anthropic-surgical"}

	rewritten, _, rejected := h.runRequestHooks(req, rec, auditRec, "req-anthropic-surgical",
		body, routingcore.RoutingTarget{}, anthropicIngress, nil, slog.Default())
	if rejected {
		t.Fatalf("the request was refused: %s", rec.Body.String())
	}
	if rewritten == nil {
		t.Fatal("no rewrite: the hook did not match, so the assertions below would prove nothing")
	}
	if bytes.Contains(rewritten, []byte("alice@example.com")) {
		t.Errorf("the email was not masked:\n%s", rewritten)
	}

	// The whole point. A rebuild would mask the text and drop all of this.
	for _, must := range []string{
		`"cache_control"`,              // prompt caching, twice over
		`"user_id":"u-42"`,             // end-user attribution
		`"type":"web_search_20250305"`, // a SERVER tool, not a client-side one
		`"max_uses":3`,
	} {
		if !bytes.Contains(rewritten, []byte(must)) {
			t.Errorf("%s is gone from the forwarded request — the redaction rebuilt the body "+
				"instead of editing it:\n%s", must, rewritten)
		}
	}
}

// The CONTROL for the case above: same policy, same content, on the ingress
// whose canonical IS its wire. This one passed before the canonical lane existed
// too — the OpenAI traffic adapter is the one adapter that could reconstruct
// masked tool-call arguments — so it proves nothing on its own. Its job is to
// show that the Anthropic result is the lane working rather than the policy
// firing differently, and that the lane did not regress the format that already
// worked.
func TestRunRequestHooks_CanonicalLaneMasksToolArgsOnOpenAIIngress(t *testing.T) {
	h := canonicalLaneHandler(t)

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"assistant","tool_calls":[` +
		`{"id":"c1","type":"function","function":{"name":"send",` +
		`"arguments":"{\"to\":\"alice@example.com\"}"}}]}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	auditRec := &audit.Record{RequestID: "req-canon-openai"}

	rewritten, _, rejected := h.runRequestHooks(req, rec, auditRec, "req-canon-openai",
		body, routingcore.RoutingTarget{}, openAIIngress, nil, slog.Default())
	if rejected {
		t.Fatalf("rejected; response=%s", rec.Body.String())
	}
	if rewritten == nil {
		t.Fatal("no rewritten body")
	}
	if bytes.Contains(rewritten, []byte("alice@example.com")) {
		t.Errorf("the tool-call argument reached the wire unmasked:\n%s", rewritten)
	}
	if !bytes.Contains(rewritten, []byte("REDACTED_EMAIL")) {
		t.Errorf("the mask is absent:\n%s", rewritten)
	}
}

// Reasoning replayed on an assistant turn is content the caller sends upstream,
// so it is content a policy must be able to mask. The flat extraction the
// request stage used before had no reasoning channel at all — the text was
// neither scanned nor maskable, on every format.
func TestRunRequestHooks_CanonicalLaneMasksReplayedReasoning(t *testing.T) {
	h := canonicalLaneHandler(t)

	body := []byte(`{"model":"deepseek-reasoner","messages":[` +
		`{"role":"user","content":"hello"},` +
		`{"role":"assistant","reasoning_content":"they wrote from alice@example.com","content":"ok"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	auditRec := &audit.Record{RequestID: "req-canon-reasoning"}

	rewritten, _, rejected := h.runRequestHooks(req, rec, auditRec, "req-canon-reasoning",
		body, routingcore.RoutingTarget{}, openAIIngress, nil, slog.Default())
	if rejected {
		t.Fatalf("rejected; response=%s", rec.Body.String())
	}
	if rewritten == nil {
		t.Fatal("no rewritten body: the reasoning channel was not scanned at all")
	}
	if bytes.Contains(rewritten, []byte("alice@example.com")) {
		t.Errorf("replayed reasoning went upstream unmasked:\n%s", rewritten)
	}
}

// The canonical lane must be unavailable — not silently half-on — when a piece
// of it is missing. Each arm returns a nil canonical body, which is what routes
// the request to the fallback extraction instead of to a rewrite that has
// nothing to rewrite.
// The lane needs the normalize registry and nothing else. It used to
// canonicalize through the bridge first; that call was an identity for every
// format the lane now serves, and removing it removed the dependency along with
// the error arm nothing could reach.
//
// Asserted rather than assumed, because "it happens to work without a bridge"
// and "it does not need one" look the same until someone reintroduces the call.
func TestCanonicalRequestForHooksNeedsNoBridge(t *testing.T) {
	h := &Handler{deps: &Deps{NormalizeRegistry: canonicalRegistry()}}
	canon, payload := h.canonicalRequestForHooks(context.Background(), provcore.FormatOpenAI,
		[]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`), slog.Default())
	if canon == nil || payload == nil {
		t.Fatal("the lane refused a well-formed chat body because no bridge was wired; the " +
			"body already IS the canonical shape and nothing needs converting")
	}
}

func TestCanonicalRequestForHooks_UnavailableArms(t *testing.T) {
	real := canonicalLaneHandler(t)
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	cases := map[string]struct {
		h    *Handler
		body []byte
	}{
		"no registry": {&Handler{deps: &Deps{
			CanonicalBridge: canonicalbridge.New(provbuiltins.SchemaCodecs(nil))}}, body},
		"empty body": {real, nil},
		// A chat body the chat codec cannot read: no messages[] at all.
		"undecodable body": {real, []byte(`{"model":"gpt-4o"}`)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			canon, payload := c.h.canonicalRequestForHooks(context.Background(),
				provcore.FormatOpenAI, c.body, slog.Default())
			if canon != nil || payload != nil {
				t.Errorf("canonical lane reported available (canon=%v payload=%v) with %s",
					canon != nil, payload != nil, name)
			}
		})
	}
}

// Every failure inside the write-back is an error, never the original bytes: the
// policy demanded a redaction, so forwarding the unredacted body would send
// upstream exactly what was masked.
func TestApplyCanonicalRequestRedaction_FailsClosed(t *testing.T) {
	h := canonicalLaneHandler(t)
	canon := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	t.Run("no payload to apply spans to", func(t *testing.T) {
		if _, _, err := h.applyCanonicalRequestRedaction(canon, nil, nil); err == nil {
			t.Fatal("a redaction with no canonical payload was accepted")
		}
	})

	t.Run("payload from a different body", func(t *testing.T) {
		_, other := h.canonicalRequestForHooks(context.Background(), provcore.FormatOpenAI,
			[]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"a"},{"role":"user","content":"b"}]}`),
			slog.Default())
		if other == nil {
			t.Fatal("setup: the control body did not decode")
		}
		if _, _, err := h.applyCanonicalRequestRedaction(canon, other, nil); err == nil {
			t.Fatal("the rewrite accepted a payload decoded from another body")
		}
	})

	t.Run("payload carrying a block no wire slot can hold", func(t *testing.T) {
		// This arm used to cover an ingress with no encoder back to its wire.
		// That failure mode went away with the encode itself — the rewrite edits
		// the caller's own bytes now — so it covers the remaining way a
		// write-back can refuse: a payload whose blocks outnumber the slots.
		_, payload := h.canonicalRequestForHooks(context.Background(), provcore.FormatOpenAI, canon, slog.Default())
		if payload == nil {
			t.Fatal("setup: the canonical body did not decode")
		}
		payload.Messages[0].Content = append(payload.Messages[0].Content,
			normcore.ContentBlock{Type: normcore.ContentText, Text: "extra"})
		if _, _, err := h.applyCanonicalRequestRedaction(canon, payload, nil); err == nil {
			t.Fatal("the rewrite accepted a payload carrying a block no wire slot could hold")
		}
	})
}

// The counter is how an operator sees whether the canonical lane is actually
// carrying traffic or quietly falling back on every request. It has to
// distinguish the two, and it has to fire on both legs of the lane — a body the
// bridge accepted but the decoder could not read counts as an error, not a
// success.
func TestCanonicalRequestForHooks_RecordsWhichLaneRan(t *testing.T) {
	newH := func(mr *trackingMetricsRecorder) *Handler {
		return &Handler{deps: &Deps{
			Metrics:           mr,
			CanonicalBridge:   canonicalbridge.New(provbuiltins.SchemaCodecs(nil)),
			NormalizeRegistry: canonicalRegistry(),
		}}
	}

	t.Run("success", func(t *testing.T) {
		mr := &trackingMetricsRecorder{}
		canon, payload := newH(mr).canonicalRequestForHooks(context.Background(), provcore.FormatOpenAI,
			[]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`), slog.Default())
		if canon == nil || payload == nil {
			t.Fatal("the canonical lane reported unavailable for a well-formed chat body")
		}
		if len(mr.extracts) != 1 || mr.extracts[0] != "success" {
			t.Errorf("outcomes = %v, want [success]", mr.extracts)
		}
	})

	t.Run("a non-OpenAI ingress is off this lane and records nothing", func(t *testing.T) {
		mr := &trackingMetricsRecorder{}
		// Off the lane BEFORE any work, so it must not spend a counter either:
		// an outcome recorded here would show up as a canonical-lane error on
		// every Anthropic request and bury the ones that are real.
		canon, _ := newH(mr).canonicalRequestForHooks(context.Background(), provcore.FormatAnthropic,
			[]byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hi"}]}`),
			slog.Default())
		if canon != nil {
			t.Fatal("a non-OpenAI ingress took the canonical lane, whose write-back rebuilds " +
				"the request and drops what canonical chat does not model")
		}
		if len(mr.extracts) != 0 {
			t.Errorf("outcomes = %v, want none — this ingress never entered the lane", mr.extracts)
		}
	})

	t.Run("decoder cannot read what the bridge passed through", func(t *testing.T) {
		mr := &trackingMetricsRecorder{}
		// OpenAI ingress canonicalizes by identity, so this body reaches the
		// decoder untouched — and a chat request with no messages[] is not one
		// the chat normalizer serves. This is the arm that separates "the bridge
		// refused" from "nothing could be decoded", which the single outcome
		// label alone cannot.
		canon, payload := newH(mr).canonicalRequestForHooks(context.Background(), provcore.FormatOpenAI,
			[]byte(`{"model":"gpt-4o","prompt":"hi"}`), slog.Default())
		if canon != nil || payload != nil {
			t.Fatal("an undecodable body was reported as canonical")
		}
		if len(mr.extracts) != 1 || mr.extracts[0] != "error" {
			t.Errorf("outcomes = %v, want [error]", mr.extracts)
		}
	})
}
