// proxy_cache_crossingress_test.go — cross-ingress shape contamination
// regression tests. Seeds the L1 cache with a ResponseEntry tagged with
// the writer's origin wire shape (OriginWireShape), then drives
// ServeProxy on a different ingress with the same canonical body
// fingerprint. The handler must reshape the cached body via
// canonicalbridge.ResponseAcrossFormats before writing to the client —
// verbatim service of the writer's shape was the bug that triggered
// this fix (chat.completion bytes served to a /v1/responses caller).
//
// Before the fix: every ingress sharing the same canonical fingerprint
// received the writer's wire shape verbatim.
// After the fix:
//   - same origin == ingress: serve verbatim (cheapest path).
//   - different origin, tagged: reshape via the two-step bridge
//     helper (DecodeResponse(origin) → canonical →
//     ResponseCanonicalToIngress(ingress)).
//   - legacy untagged entry (OriginWireShape == ""): fall back to
//     canonical-assuming reshape so pre-fix entries continue to
//     work until they TTL-expire.
package proxy

import (
	"context"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// Cached chat.completion body the seeded entry will hold.
const crossIngressCachedChatBody = `{` +
	`"id":"chatcmpl_seeded",` +
	`"object":"chat.completion",` +
	`"created":1747353700,` +
	`"model":"gpt-4o",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"cross-ingress seeded reply"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":4,"completion_tokens":5,"total_tokens":9}}`

// Cached Responses-API body for the reverse-direction case.
const crossIngressCachedResponsesBody = `{` +
	`"id":"resp_seeded",` +
	`"object":"response",` +
	`"created_at":1747353700,` +
	`"model":"gpt-4o",` +
	`"status":"completed",` +
	`"output":[{"id":"msg_0","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"seeded responses reply"}]}],` +
	`"usage":{"input_tokens":4,"output_tokens":5,"total_tokens":9}}`

// requestBodyForCrossIngress builds the per-ingress request body. The
// canonical fingerprint (provider+model+canonicalizeJSON(body)) is the
// cache key input. To force a HIT on the seeded entry we precompute the
// cache key for the matching ingress's PrepareBody output and seed at
// that key directly (since cross-ingress writers produce different
// canonical bodies — different field names — and the same key only
// arises when each ingress's PrepareBody happens to coincide). For
// these tests we sidestep cross-ingress key equality entirely: we
// compute the cache key from the SAME ingress as the reader and merely
// vary the entry's OriginWireShape tag. This isolates
// the reshape gate (the unit under test) from cache-key fingerprint
// equality (which is a separate property).

// seedResponseEntry stores entry at the cache key derived for the
// reader's ingress + body. Returns the key for diagnostic logging.
//
// The handler derives the key from PrepareBody(finalBody). For OpenAI
// non-streaming requests PrepareBody is effectively identity so the raw
// request body matches. The streaming-cache helper (computeStreamCacheKey)
// is not appropriate here because it forces Stream=true and
// stream_options.include_usage injection — neither applies to the
// non-stream cache HIT path under test.
func seedResponseEntry(t *testing.T, deps *Deps, model string, requestBody []byte, entry *cache.ResponseEntry) string {
	t.Helper()
	key := deps.Cache.BuildKey("openai", model, requestBody, "")
	if _, err := deps.Cache.StoreResponse(context.Background(), key, entry); err != nil {
		t.Fatalf("StoreResponse: %v", err)
	}
	return key
}

// TestCacheHit_OriginSameAsIngress_NoReshape verifies the same-shape
// fast path: when the entry's OriginWireShape matches
// the ingress, the cache HIT reader serves the body verbatim with no
// reshape. The cached body's exact bytes flow to the client.
func TestCacheHit_OriginSameAsIngress_NoReshape(t *testing.T) {
	cacheOpt, cleanup := withCache(t)
	defer cleanup()

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"same-shape"}]}`)
	deps := makeOpenAIDeps(t, "", emptyHookCache(t), cacheOpt)

	entry := &cache.ResponseEntry{
		Provider:          "openai",
		Model:             "gpt-4o",
		CanonicalResponse: json.RawMessage(crossIngressCachedChatBody),
		Usage:             provcore.Usage{PromptTokens: iPtr(4), CompletionTokens: iPtr(5), TotalTokens: iPtr(9)},
		CachedAt:          time.Now().UTC(),
		OriginWireShape:   typology.WireShapeOpenAIChat,
	}
	seedResponseEntry(t, deps, "gpt-4o", body, entry)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 on cache HIT; body=%s", w.Code, w.Body.String())
	}
	if !strings.EqualFold(w.Header().Get("X-Nexus-Cache"), "hit") {
		t.Errorf("x-nexus-cache header = %q, want hit", w.Header().Get("X-Nexus-Cache"))
	}
	// Same-shape: body served verbatim (object stays chat.completion).
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not valid JSON: %v; body=%s", err, w.Body.String())
	}
	if got["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion (same-shape no-reshape); body=%s", got["object"], w.Body.String())
	}
	if _, has := got["choices"]; !has {
		t.Errorf("choices[] missing; body=%s", w.Body.String())
	}
}

// TestCacheHit_OriginChat_IngressResponses_Reshapes is the canonical
// cross-ingress contamination scenario: chat.completion-shape body is
// in the cache (tagged OriginWireShape=openai-chat)
// but the request comes in on /v1/responses (Endpoint=ResponsesAPI /
// BodyFormat=OpenAIResponses). The HIT reader must call
// ResponseAcrossFormats to reshape; the client receives `object:response`
// + `output[]`, not `object:chat.completion` + `choices[]`.
func TestCacheHit_OriginChat_IngressResponses_Reshapes(t *testing.T) {
	cacheOpt, cleanup := withCache(t)
	defer cleanup()

	// Responses-API request shape (ingress=/v1/responses): uses `input`
	// instead of `messages`.
	body := []byte(`{"model":"gpt-4o","input":"cross ingress contamination"}`)
	deps := makeOpenAIDeps(t, "", emptyHookCache(t), cacheOpt)

	entry := &cache.ResponseEntry{
		Provider:          "openai",
		Model:             "gpt-4o",
		CanonicalResponse: json.RawMessage(crossIngressCachedChatBody),
		Usage:             provcore.Usage{PromptTokens: iPtr(4), CompletionTokens: iPtr(5), TotalTokens: iPtr(9)},
		CachedAt:          time.Now().UTC(),
		OriginWireShape:   typology.WireShapeOpenAIChat,
	}
	seedResponseEntry(t, deps, "gpt-4o", body, entry)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIResponses,
		BodyFormat: provcore.FormatOpenAIResponses,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.EqualFold(w.Header().Get("X-Nexus-Cache"), "hit") {
		t.Errorf("x-nexus-cache header = %q, want hit", w.Header().Get("X-Nexus-Cache"))
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not valid JSON: %v; body=%s", err, w.Body.String())
	}
	if got["object"] != "response" {
		t.Errorf("object = %v, want response (post-reshape); body=%s", got["object"], w.Body.String())
	}
	if _, hasOutput := got["output"]; !hasOutput {
		t.Errorf("output[] missing on reshaped body; body=%s", w.Body.String())
	}
	if _, hasChoices := got["choices"]; hasChoices {
		t.Errorf("choices[] should NOT survive reshape; body=%s", w.Body.String())
	}
}

// TestCacheHit_OriginResponses_IngressChat_Reshapes is the reverse case:
// the cached entry was written by /v1/responses (object=response,
// output[]) and the request comes in on /v1/chat/completions. The HIT
// reader must reshape so the client sees object=chat.completion +
// choices[].
func TestCacheHit_OriginResponses_IngressChat_Reshapes(t *testing.T) {
	cacheOpt, cleanup := withCache(t)
	defer cleanup()

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"reverse cross-ingress"}]}`)
	deps := makeOpenAIDeps(t, "", emptyHookCache(t), cacheOpt)

	entry := &cache.ResponseEntry{
		Provider:          "openai",
		Model:             "gpt-4o",
		CanonicalResponse: json.RawMessage(crossIngressCachedResponsesBody),
		Usage:             provcore.Usage{PromptTokens: iPtr(4), CompletionTokens: iPtr(5), TotalTokens: iPtr(9)},
		CachedAt:          time.Now().UTC(),
		OriginWireShape:   typology.WireShapeOpenAIResponses,
	}
	seedResponseEntry(t, deps, "gpt-4o", body, entry)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not valid JSON: %v; body=%s", err, w.Body.String())
	}
	if got["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion (post-reshape); body=%s", got["object"], w.Body.String())
	}
	if _, has := got["choices"]; !has {
		t.Errorf("choices[] missing on reshaped body; body=%s", w.Body.String())
	}
	if _, has := got["output"]; has {
		t.Errorf("output[] should NOT survive reshape; body=%s", w.Body.String())
	}
}

// TestCacheHit_Stream_OriginDiffers_StampsContext exercises the B2
// stream-HIT branch in handleStreamHit: a tagged stream entry whose
// origin shape differs from the current ingress causes
// WithStreamHitOrigin to stamp the override on the request context.
// Re-uses the proxy_serveproxy_test stream-HIT seeding pattern
// (chat-completions ingress + streaming body) so the cache key
// derivation aligns with computeStreamCacheKey, then tags the entry
// with a non-matching origin to trigger the override path.
func TestCacheHit_Stream_OriginDiffers_StampsContext(t *testing.T) {
	cacheOpt, cleanup := withCache(t)
	defer cleanup()

	body := []byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"b2 stream HIT diff origin"}]}`)
	deps := makeOpenAIDeps(t, "", emptyHookCache(t), cacheOpt)

	streamEntry := &cache.StreamEntry{
		Provider: "openai",
		Model:    "gpt-4o",
		Chunks: []cache.ChunkRecord{
			{Delta: "from "},
			{Delta: "B2 override path"},
			{Done: true, Usage: &provcore.Usage{
				PromptTokens: iPtr(3), CompletionTokens: iPtr(4), TotalTokens: iPtr(7),
			}},
		},
		CachedAt:        time.Now().UTC(),
		OriginWireShape: typology.WireShapeOpenAIResponses,
	}
	cacheKey := computeStreamCacheKey(t, deps, "openai", "gpt-4o", body, true)
	if _, err := deps.Cache.StoreStream(context.Background(), cacheKey, streamEntry); err != nil {
		t.Fatalf("StoreStream: %v", err)
	}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
		Stream:     true,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.EqualFold(w.Header().Get("X-Nexus-Cache"), "hit") {
		t.Errorf("x-nexus-cache=%q want HIT", w.Header().Get("X-Nexus-Cache"))
	}
	// Body should still carry the replayed deltas. The exact wire
	// shape depends on the override-selected transcoder; the test
	// asserts the branch ran end-to-end without errors and the cached
	// content surfaced.
	out := w.Body.String()
	if !strings.Contains(out, "B2 override path") {
		t.Errorf("cached delta missing from replay: %s", out)
	}
}

// TestCacheHit_Stream_CrossIngress_WireGrammar is the Amendment E
// streaming proof, both directions: a stream cache entry written under
// one OpenAI wire shape and replayed to a client on the OTHER shape must
// reach the client re-encoded into the READER's SSE grammar — never the
// writer's. Replay ChunkRecords carry no RawBytes/Verbatim, so the
// response-side content-peek classifier cannot fire on a HIT; the
// OriginWireShape tag stamped on the request context (StreamHitOrigin) is
// the ONLY signal that selects the transcoder in streamShapeStage. This
// pins that the origin override survived the Format-backdoor deletion:
// the target-Format reassignment now drives NewStreamTranscoder to the
// reader's encoder in both directions (chat→responses and responses→chat).
func TestCacheHit_Stream_CrossIngress_WireGrammar(t *testing.T) {
	const relayToken = "xrelaymarker"
	cases := []struct {
		name          string
		originShape   typology.WireShape
		ingressShape  typology.WireShape
		ingressFormat provcore.Format
		path          string
		reqBody       []byte
		keyWireShape  typology.WireShape
		wantContains  []string
		wantAbsent    []string
	}{
		{
			name:          "chat_written_responses_replay",
			originShape:   typology.WireShapeOpenAIChat,
			ingressShape:  typology.WireShapeOpenAIResponses,
			ingressFormat: provcore.FormatOpenAIResponses,
			path:          "/v1/responses",
			reqBody:       []byte(`{"model":"gpt-4o","input":"cross stream chat to responses","stream":true}`),
			keyWireShape:  typology.WireShapeOpenAIResponses,
			// The Responses encoder opens with response.created and closes
			// with response.completed; the cached delta rides an
			// output_text.delta event in between.
			wantContains: []string{"event: response.created", "event: response.completed", relayToken},
			wantAbsent:   []string{"chat.completion.chunk"},
		},
		{
			name:          "responses_written_chat_replay",
			originShape:   typology.WireShapeOpenAIResponses,
			ingressShape:  typology.WireShapeOpenAIChat,
			ingressFormat: provcore.FormatOpenAI,
			path:          "/v1/chat/completions",
			reqBody:       []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"cross stream responses to chat"}],"stream":true}`),
			keyWireShape:  typology.WireShapeOpenAIChat,
			wantContains:  []string{"chat.completion.chunk", relayToken},
			wantAbsent:    []string{"event: response.created", "event: response.completed"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cacheOpt, cleanup := withCache(t)
			defer cleanup()

			deps := makeOpenAIDeps(t, "", emptyHookCache(t), cacheOpt)
			streamEntry := &cache.StreamEntry{
				Provider: "openai",
				Model:    "gpt-4o",
				Chunks: []cache.ChunkRecord{
					{Delta: relayToken},
					{Done: true, Usage: &provcore.Usage{
						PromptTokens: iPtr(2), CompletionTokens: iPtr(3), TotalTokens: iPtr(5),
					}},
				},
				CachedAt:        time.Now().UTC(),
				OriginWireShape: tc.originShape,
			}
			// Derive the cache key exactly as the handler does for this
			// reader: PrepareBody at the reader's wire shape, then BuildKey.
			adapter, ok := deps.ProviderReg.Get(provcore.FormatOpenAI)
			if !ok {
				t.Fatal("openai adapter missing")
			}
			prepReq := provcore.Request{
				WireShape:  tc.keyWireShape,
				Body:       tc.reqBody,
				BodyFormat: tc.ingressFormat,
				Stream:     true,
			}
			prepReq.Target.ProviderModelID = "gpt-4o"
			finalBody, _, _, err := adapter.PrepareBody(prepReq)
			if err != nil {
				t.Fatalf("PrepareBody: %v", err)
			}
			cacheKey := deps.Cache.BuildKey("openai", "gpt-4o", finalBody, "")
			if _, err := deps.Cache.StoreStream(context.Background(), cacheKey, streamEntry); err != nil {
				t.Fatalf("StoreStream: %v", err)
			}

			h := NewHandler(deps).ServeProxy(Ingress{
				WireShape:  tc.ingressShape,
				BodyFormat: tc.ingressFormat,
				Stream:     true,
			})
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(string(tc.reqBody)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer vk")
			w := httptest.NewRecorder()
			h(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if !strings.EqualFold(w.Header().Get("X-Nexus-Cache"), "hit") {
				t.Fatalf("x-nexus-cache=%q want HIT (cross-ingress stream replay did not hit)", w.Header().Get("X-Nexus-Cache"))
			}
			out := w.Body.String()
			for _, want := range tc.wantContains {
				if !strings.Contains(out, want) {
					t.Errorf("replay missing %q — origin override did not re-encode to the reader's grammar; body=%s", want, out)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(out, absent) {
					t.Errorf("replay leaked the writer's grammar %q to the reader; body=%s", absent, out)
				}
			}
		})
	}
}

// TestCacheHit_Stream_ResponsesIngress_OriginOverride exercises the
// B2 stream-HIT override branch in handleStreamWithSubscription:
// when origin override is set AND the standard NewStreamTranscoder
// returns nil (because target natively serves the requested ingress
// — the passthrough exception that produced the bug), the handler
// forces NewResponsesStreamEncoder so cached canonical chunks are
// re-encoded into Responses-API SSE event grammar.
func TestCacheHit_Stream_ResponsesIngress_OriginOverride(t *testing.T) {
	cacheOpt, cleanup := withCache(t)
	defer cleanup()

	// /v1/responses streaming body. Use input field (Responses-API
	// request shape) so PrepareBody on OpenAI target yields a body
	// the cache key derivation can find.
	body := []byte(`{"model":"gpt-4o","input":"stream override path","stream":true}`)
	deps := makeOpenAIDeps(t, "", emptyHookCache(t), cacheOpt)

	streamEntry := &cache.StreamEntry{
		Provider: "openai",
		Model:    "gpt-4o",
		Chunks: []cache.ChunkRecord{
			{Delta: "responses "},
			{Delta: "override delta"},
			{Done: true, Usage: &provcore.Usage{
				PromptTokens: iPtr(2), CompletionTokens: iPtr(3), TotalTokens: iPtr(5),
			}},
		},
		CachedAt:        time.Now().UTC(),
		OriginWireShape: typology.WireShapeOpenAIChat,
	}
	// Build the cache key the handler will look up. The /v1/responses
	// path passes the body through OpenAI's PrepareBody at Endpoint
	// ResponsesAPI; in the stub router (target.AdapterType=openai) the
	// adapter is found via provcore.Format("openai").
	adapter, ok := deps.ProviderReg.Get(provcore.FormatOpenAI)
	if !ok {
		t.Fatal("openai adapter missing")
	}
	prepReq := provcore.Request{
		WireShape:  typology.WireShapeOpenAIResponses,
		Body:       body,
		BodyFormat: provcore.FormatOpenAIResponses,
		Stream:     true,
	}
	prepReq.Target.ProviderModelID = "gpt-4o"
	finalBody, _, _, err := adapter.PrepareBody(prepReq)
	if err != nil {
		t.Fatalf("PrepareBody: %v", err)
	}
	cacheKey := deps.Cache.BuildKey("openai", "gpt-4o", finalBody, "")
	if _, err := deps.Cache.StoreStream(context.Background(), cacheKey, streamEntry); err != nil {
		t.Fatalf("StoreStream: %v", err)
	}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIResponses,
		BodyFormat: provcore.FormatOpenAIResponses,
		Stream:     true,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	// Override path should re-encode into Responses-API SSE event
	// grammar; absence of the chat.completion shape is the structural
	// invariant. The exact event names emitted depend on the
	// responsesStreamEncoder's state machine but they all start with
	// "response." prefix.
	if strings.Contains(out, "chat.completion.chunk") {
		t.Errorf("stream HIT body contains chat.completion.chunk shape — override did not fire; body=%s", out)
	}
}

// TestStreamHitOrigin_ContextRoundtrip pins the WithStreamHitOrigin /
// StreamHitOriginFromContext context-key plumbing used by the stream-
// HIT cross-ingress reshape override. Tests both presence and absence.
func TestStreamHitOrigin_ContextRoundtrip(t *testing.T) {
	ctx := context.Background()
	if _, ok := StreamHitOriginFromContext(ctx); ok {
		t.Error("StreamHitOriginFromContext on bare ctx should return ok=false")
	}
	want := StreamHitOrigin{
		WireShape: typology.WireShapeOpenAIChat,
	}
	ctx = WithStreamHitOrigin(ctx, want)
	got, ok := StreamHitOriginFromContext(ctx)
	if !ok {
		t.Fatal("expected ok=true after WithStreamHitOrigin")
	}
	if got != want {
		t.Errorf("roundtrip mismatch: got %+v want %+v", got, want)
	}
}

// TestCacheHit_OriginAcrossFormats_BridgeError_ServesEntryBytes
// exercises the error fallback in handleNonStreamHit: when
// ResponseAcrossFormats returns an error (e.g. the cached body claims
// to be a wire format with no codec), the reader logs a warning and
// serves the original entry bytes. The client still gets a parseable
// body — just in the wrong wire shape — which is strictly less broken
// than failing the HIT entirely.
func TestCacheHit_OriginAcrossFormats_BridgeError_ServesEntryBytes(t *testing.T) {
	cacheOpt, cleanup := withCache(t)
	defer cleanup()

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"bridge-error scenario"}]}`)
	deps := makeOpenAIDeps(t, "", emptyHookCache(t), cacheOpt)

	// Entry's origin format has no registered codec, so the bridge's
	// ResponseAcrossFormats returns an error and the reader falls
	// through to serving the original bytes verbatim.
	original := json.RawMessage(`{"bizarre":"origin","payload":"verbatim"}`)
	entry := &cache.ResponseEntry{
		Provider:          "openai",
		Model:             "gpt-4o",
		CanonicalResponse: original,
		Usage:             provcore.Usage{PromptTokens: iPtr(1), CompletionTokens: iPtr(1), TotalTokens: iPtr(2)},
		CachedAt:          time.Now().UTC(),
		OriginWireShape:   typology.WireShape("not-a-real-wire-shape-xyz"),
	}
	seedResponseEntry(t, deps, "gpt-4o", body, entry)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (fallback should still succeed); body=%s", w.Code, w.Body.String())
	}
	// Body should be the original entry bytes — verbatim. Confirms the
	// reader logged the bridge error and did not crash or replace the
	// body with an error envelope.
	if !strings.Contains(w.Body.String(), `"bizarre":"origin"`) {
		t.Errorf("expected fallback to serve original entry bytes; got %s", w.Body.String())
	}
}

// TestCacheHit_ResponsesCross_StoredCanonical_ReshapesToOutput is the B1
// regression: a CROSS-format /v1/responses request canonicalises to chat before
// caching, so the stored body is canonical chat (`choices[]`) yet the entry is
// tagged OriginWireShape=openai-responses (same tag native passthrough uses for
// a responses-shape body). The HIT reader must detect the canonical body and
// re-encode it to the /v1/responses reader's `output[]` — NOT serve `choices[]`
// verbatim to a Responses SDK.
func TestCacheHit_ResponsesCross_StoredCanonical_ReshapesToOutput(t *testing.T) {
	cacheOpt, cleanup := withCache(t)
	defer cleanup()

	body := []byte(`{"model":"gpt-4o","input":"responses cross cache"}`)
	deps := makeOpenAIDeps(t, "", emptyHookCache(t), cacheOpt)

	// Cross-format responses entry: CANONICAL chat body, tagged responses.
	entry := &cache.ResponseEntry{
		Provider:          "openai",
		Model:             "gpt-4o",
		CanonicalResponse: json.RawMessage(crossIngressCachedChatBody),
		Usage:             provcore.Usage{PromptTokens: iPtr(4), CompletionTokens: iPtr(5), TotalTokens: iPtr(9)},
		CachedAt:          time.Now().UTC(),
		OriginWireShape:   typology.WireShapeOpenAIResponses,
	}
	seedResponseEntry(t, deps, "gpt-4o", body, entry)

	h := NewHandler(deps).ServeProxy(Ingress{WireShape: typology.WireShapeOpenAIResponses, BodyFormat: provcore.FormatOpenAIResponses})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d; body=%s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v; body=%s", err, w.Body.String())
	}
	if _, has := got["output"]; !has {
		t.Errorf("responses-cross HIT must reshape canonical→output[]; got %s", w.Body.String())
	}
	if _, has := got["choices"]; has {
		t.Errorf("canonical choices[] leaked to a /v1/responses caller on HIT; body=%s", w.Body.String())
	}
}

// TestCacheHit_ResponsesNative_StoredResponsesShape_Verbatim verifies the other
// side: a NATIVE /v1/responses entry stores responses-shape (`output[]`, no
// `choices`); a /v1/responses reader receives it verbatim (no re-encode that
// would strip output[].content[].text).
func TestCacheHit_ResponsesNative_StoredResponsesShape_Verbatim(t *testing.T) {
	cacheOpt, cleanup := withCache(t)
	defer cleanup()

	body := []byte(`{"model":"gpt-4o","input":"responses native cache"}`)
	deps := makeOpenAIDeps(t, "", emptyHookCache(t), cacheOpt)

	entry := &cache.ResponseEntry{
		Provider:          "openai",
		Model:             "gpt-4o",
		CanonicalResponse: json.RawMessage(crossIngressCachedResponsesBody),
		Usage:             provcore.Usage{PromptTokens: iPtr(4), CompletionTokens: iPtr(5), TotalTokens: iPtr(9)},
		CachedAt:          time.Now().UTC(),
		OriginWireShape:   typology.WireShapeOpenAIResponses,
	}
	seedResponseEntry(t, deps, "gpt-4o", body, entry)

	h := NewHandler(deps).ServeProxy(Ingress{WireShape: typology.WireShapeOpenAIResponses, BodyFormat: provcore.FormatOpenAIResponses})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"output"`) || strings.Contains(w.Body.String(), `"choices"`) {
		t.Errorf("responses-native HIT must serve responses-shape output[] verbatim; got %s", w.Body.String())
	}
}

// TestCacheHit_LegacyUntaggedEntry_FallsBackToOldBehavior verifies the
// legacy entry path. An entry with empty OriginWireShape (untagged
// write) preserves the pre-fix reshape semantics: the gate runs only
// for (chat-completions ingress) OR (responses ingress + target does
// NOT natively serve Responses). Untagged entries did NOT reshape on
// /v1/responses + OpenAI native target — this is the documented
// original behavior and is the reason new entries must be tagged.
// Legacy untagged entries during TTL rollover retain their original
// (possibly contaminated) shape — accepted by the design as a
// transient cost; new writes carry the tag and reshape correctly.
//
// We assert here that the legacy reshape branch fires for the
// non-native target case (chat-completions ingress on an OpenAI target
// — the gate condition is true). The body should be served as-is
// because canonical → chat-completions reshape is an identity for an
// OpenAI ingress.
func TestCacheHit_LegacyUntaggedEntry_FallsBackToOldBehavior(t *testing.T) {
	cacheOpt, cleanup := withCache(t)
	defer cleanup()

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"legacy entry"}]}`)
	deps := makeOpenAIDeps(t, "", emptyHookCache(t), cacheOpt)

	// Legacy entry — OriginWireShape unset (zero value).
	entry := &cache.ResponseEntry{
		Provider:          "openai",
		Model:             "gpt-4o",
		CanonicalResponse: json.RawMessage(crossIngressCachedChatBody),
		Usage:             provcore.Usage{PromptTokens: iPtr(4), CompletionTokens: iPtr(5), TotalTokens: iPtr(9)},
		CachedAt:          time.Now().UTC(),
	}
	seedResponseEntry(t, deps, "gpt-4o", body, entry)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	// Legacy code path ran the canonical→OpenAI reshape, which is
	// identity on OpenAI ingress — body stays chat.completion.
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not valid JSON: %v; body=%s", err, w.Body.String())
	}
	if got["object"] != "chat.completion" {
		t.Errorf("legacy fallback should retain chat.completion shape on same-ingress; object = %v", got["object"])
	}
}
