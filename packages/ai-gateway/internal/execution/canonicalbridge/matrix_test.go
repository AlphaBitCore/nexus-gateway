package canonicalbridge

import (
	"github.com/goccy/go-json"
	"log/slog"
	"testing"

	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

func testBridge(t *testing.T) *Bridge {
	t.Helper()
	return New(provbuiltins.SchemaCodecs(slog.Default()))
}

func dummyCallTarget(f provcore.Format) provcore.CallTarget {
	return provcore.CallTarget{
		Format:          f,
		ProviderModelID: FixtureProviderModel(f),
	}
}

func minimalNativeChatRequest(t *testing.T, ingress provcore.Format) []byte {
	t.Helper()
	body, err := MinimalNativeChatBody(ingress)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return body
}

// minimalCanonicalChatResponse is a valid OpenAI chat.completion JSON used
// to exercise ResponseCanonicalToIngress for native ingresses.
func minimalCanonicalChatResponse() []byte {
	return []byte(`{
		"id": "chatcmpl-test",
		"object": "chat.completion",
		"created": 1700000000,
		"model": "routed-model",
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "content": "ok"},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
	}`)
}

// TestIngressChatToWire_StreamIntentReachesNonOpenAITarget is the regression for
// the gemini-ingress → anthropic-target streaming text-loss bug. Gemini signals
// streaming via the :streamGenerateContent URL, so its canonical chat body has
// no `stream` field. The Anthropic codec propagates `stream` from its canonical
// input; without the stream stamp the upstream /v1/messages request is
// non-streaming and the client's SSE terminates with EMPTY text. The stamp must
// flow the resolved stream intent into the target wire body.
func TestIngressChatToWire_StreamIntentReachesNonOpenAITarget(t *testing.T) {
	b := testBridge(t)
	body := minimalNativeChatRequest(t, provcore.FormatGemini)
	ct := dummyCallTarget(provcore.FormatAnthropic)

	streamed, err := b.IngressChatToWire(provcore.FormatGemini, provcore.FormatAnthropic, body, ct, true)
	if err != nil {
		t.Fatalf("IngressChatToWire stream=true: %v", err)
	}
	if !gjson.GetBytes(streamed, "stream").Bool() {
		t.Errorf("stream=true must stamp anthropic body stream:true; got %s", streamed)
	}

	nonStreamed, err := b.IngressChatToWire(provcore.FormatGemini, provcore.FormatAnthropic, body, ct, false)
	if err != nil {
		t.Fatalf("IngressChatToWire stream=false: %v", err)
	}
	if gjson.GetBytes(nonStreamed, "stream").Bool() {
		t.Errorf("stream=false must NOT set anthropic stream:true; got %s", nonStreamed)
	}
}

func TestEnsureCanonicalStream(t *testing.T) {
	// Stamps stream onto an OpenAI-canonical body that lacks it.
	got := EnsureCanonicalStream([]byte(`{"model":"m","messages":[]}`))
	if !gjson.GetBytes(got, "stream").Bool() {
		t.Errorf("stream not stamped: %s", got)
	}
	// Empty body is returned unchanged (no panic, no spurious bytes).
	if out := EnsureCanonicalStream(nil); len(out) != 0 {
		t.Errorf("empty body must stay empty; got %s", out)
	}
}

func TestIngressChatToWire_PassthroughEveryFormat(t *testing.T) {
	b := testBridge(t)
	for _, ingress := range provcore.AllFormats() {
		// Skip embeddings-only formats (Voyage, future audio/image-only
		// providers) — they have no chat fixture and ChatRoutable rejects
		// them as chat targets. The embeddings matrix at
		// TestEndpointRoutable_EmbeddingsMatrix covers their wire path.
		if !b.ChatRoutable(ingress, ingress) {
			continue
		}
		t.Run(string(ingress), func(t *testing.T) {
			body := minimalNativeChatRequest(t, ingress)
			ct := dummyCallTarget(ingress)
			out, err := b.IngressChatToWire(ingress, ingress, body, ct, false)
			if err != nil {
				t.Fatalf("passthrough: %v", err)
			}
			if len(out) == 0 {
				t.Fatal("empty passthrough body")
			}
			if !json.Valid(out) {
				t.Fatalf("invalid JSON: %s", string(out))
			}
		})
	}
}

// TestIngressChatToWire_AllChatRoutableCrossPairs ensures every ingress/target
// pair allowed by ChatRoutable (excluding pure passthrough) produces valid
// upstream wire JSON via the hub + SchemaCodec.EncodeRequest path.
func TestIngressChatToWire_AllChatRoutableCrossPairs(t *testing.T) {
	b := testBridge(t)
	for _, ingress := range provcore.AllFormats() {
		for _, target := range provcore.AllFormats() {
			if ingress == target {
				continue
			}
			if !b.ChatRoutable(ingress, target) {
				continue
			}
			t.Run(string(ingress)+"_to_"+string(target), func(t *testing.T) {
				body := minimalNativeChatRequest(t, ingress)
				ct := dummyCallTarget(target)
				wire, err := b.IngressChatToWire(ingress, target, body, ct, false)
				if err != nil {
					t.Fatalf("IngressChatToWire: %v", err)
				}
				if len(wire) == 0 {
					t.Fatal("empty wire body")
				}
				if !json.Valid(wire) {
					t.Fatalf("invalid JSON wire: %s", string(wire))
				}
				// Spot-check a few high-value transforms.
				switch target {
				case provcore.FormatBedrock:
					if gjson.GetBytes(wire, "anthropic_version").String() == "" {
						t.Fatalf("bedrock wire missing anthropic_version: %s", string(wire))
					}
				case provcore.FormatGemini, provcore.FormatVertex:
					if !gjson.GetBytes(wire, "contents").Exists() {
						t.Fatalf("gemini/vertex wire missing contents: %s", string(wire))
					}
				case provcore.FormatAnthropic:
					if !gjson.GetBytes(wire, "messages").Exists() {
						t.Fatalf("anthropic wire missing messages: %s", string(wire))
					}
				}
			})
		}
	}
}

func TestIngressChatToCanonical_RejectsUnsupportedIngress(t *testing.T) {
	b := testBridge(t)
	ct := dummyCallTarget(provcore.FormatOpenAI)
	body := minimalNativeChatRequest(t, provcore.FormatBedrock)
	_, err := b.IngressChatToCanonical(provcore.FormatBedrock, body, ct)
	if err == nil {
		t.Fatal("expected error for bedrock ingress (no dedicated ingress→canonical mapper; passthrough is same-format only)")
	}
}

func TestResponseCanonicalToIngress_NativeShapes(t *testing.T) {
	b := testBridge(t)
	canon := minimalCanonicalChatResponse()

	t.Run("anthropic", func(t *testing.T) {
		out, err := b.ResponseCanonicalToIngress(provcore.FormatAnthropic, canon)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(out, "type").String() != "message" {
			t.Fatalf("expected Anthropic message response, got %s", string(out))
		}
		if !gjson.GetBytes(out, "content").Exists() {
			t.Fatal("missing content")
		}
	})

	t.Run("gemini", func(t *testing.T) {
		out, err := b.ResponseCanonicalToIngress(provcore.FormatGemini, canon)
		if err != nil {
			t.Fatal(err)
		}
		if !gjson.GetBytes(out, "candidates").Exists() {
			t.Fatalf("expected Gemini candidates: %s", string(out))
		}
	})

	t.Run("vertex", func(t *testing.T) {
		out, err := b.ResponseCanonicalToIngress(provcore.FormatVertex, canon)
		if err != nil {
			t.Fatal(err)
		}
		if !gjson.GetBytes(out, "candidates").Exists() {
			t.Fatalf("expected Vertex/Gemini-shaped candidates: %s", string(out))
		}
	})

	for _, f := range []provcore.Format{
		provcore.FormatOpenAI,
		provcore.FormatDeepSeek,
		provcore.FormatGLM,
		provcore.FormatAzureOpenAI,
	} {
		t.Run("openAILike_"+string(f), func(t *testing.T) {
			out, err := b.ResponseCanonicalToIngress(f, canon)
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != string(canon) {
				t.Fatal("openAI-like ingress must return canonical bytes unchanged")
			}
		})
	}
}

// inScopeEmbeddingFormat captures the in-scope embedding wire shapes
// (OpenAI canonical + sibling OpenAI-wire-shape compats, plus Cohere and
// Gemini family) — keep in lockstep with Bridge.EmbeddingsRoutable.
func inScopeEmbeddingFormat(f provcore.Format) bool {
	if f.IsOpenAIFamily() {
		return true
	}
	switch f {
	case provcore.FormatCohere, provcore.FormatGemini, provcore.FormatVertex:
		return true
	}
	return false
}

// TestEndpointRoutable_EmbeddingsMatrix pins the contract: cross-format
// routing is allowed for every (ingress, target) pair where both sides are
// in-scope embedding wire shapes and the bridge has a codec registered.
// Out-of-scope formats (e.g. Bedrock, Replicate) are still rejected.
func TestEndpointRoutable_EmbeddingsMatrix(t *testing.T) {
	b := testBridge(t)
	ep := typology.WireShapeOpenAIEmbeddings
	for _, ingress := range provcore.AllFormats() {
		for _, target := range provcore.AllFormats() {
			got := b.EndpointRoutable(ep, ingress, target)
			want := ingress == target ||
				(inScopeEmbeddingFormat(ingress) && inScopeEmbeddingFormat(target))
			if got != want {
				t.Errorf("EndpointRoutable(embeddings,%s,%s)=%v want %v", ingress, target, got, want)
			}
		}
	}
}

func TestEndpointRoutable_ModelsMatrix(t *testing.T) {
	// /v1/models routing has the same "same-format only" constraint as
	// embeddings — there's no canonical translation pipeline for the
	// list-models payload shape.
	b := testBridge(t)
	for _, ingress := range provcore.AllFormats() {
		for _, target := range provcore.AllFormats() {
			got := b.EndpointRoutable(typology.WireShapeNone, ingress, target)
			// Same-format always routes. OpenAI ingress routes anywhere
			// (legacy translation rule). Everything else is same-format-only.
			want := ingress == target || ingress == provcore.FormatOpenAI
			if got != want {
				t.Errorf("EndpointRoutable(models,%s,%s)=%v want %v", ingress, target, got, want)
			}
		}
	}
}

func TestEndpointRoutable_CompletionsLegacyMatrix(t *testing.T) {
	// Same-format-only invariant for the legacy /v1/completions endpoint.
	b := testBridge(t)
	for _, ingress := range provcore.AllFormats() {
		for _, target := range provcore.AllFormats() {
			got := b.EndpointRoutable(typology.WireShapeOpenAICompletionsLegacy, ingress, target)
			// Same legacy-OpenAI translation rule as /v1/models.
			want := ingress == target || ingress == provcore.FormatOpenAI
			if got != want {
				t.Errorf("EndpointRoutable(completions-legacy,%s,%s)=%v want %v", ingress, target, got, want)
			}
		}
	}
}

func TestEndpointRoutable_ResponsesAPIRoutes(t *testing.T) {
	// /v1/responses ingress (FormatOpenAIResponses) must be routable to
	// every Valid() target except Bedrock (no SSE on streams). Same-
	// format passthrough always works. Other ingress formats are NOT
	// allowed (Responses is a strict OpenAI-only ingress).
	b := testBridge(t)
	ep := typology.WireShapeOpenAIResponses
	for _, ingress := range provcore.AllFormats() {
		for _, target := range provcore.AllFormats() {
			got := b.EndpointRoutable(ep, ingress, target)
			want := ingress == provcore.FormatOpenAIResponses && b.ResponsesRoutable(target)
			if got != want {
				t.Errorf("EndpointRoutable(responses,%s,%s)=%v want %v", ingress, target, got, want)
			}
		}
	}
}

func TestSubsetFields_CoversCanonicalContract(t *testing.T) {
	// SubsetFields documents the canonical hub's exposed field set —
	// consumed by self-check scaffolding and SDD-driven documentation
	// generators. The test pins core canonical fields so a refactor
	// that drops one (e.g. "messages") is caught instead of silently
	// reducing the canonical surface.
	req, resp, stream := SubsetFields()

	mustContain := func(t *testing.T, name string, list []string, key string) {
		t.Helper()
		for _, k := range list {
			if k == key {
				return
			}
		}
		t.Errorf("%s subset missing %q; got %v", name, key, list)
	}

	for _, key := range []string{"model", "messages", "temperature", "stream", "tools"} {
		mustContain(t, "request", req, key)
	}
	for _, key := range []string{"id", "model", "choices", "usage"} {
		mustContain(t, "response", resp, key)
	}
	for _, key := range []string{"id", "model", "choices"} {
		mustContain(t, "stream", stream, key)
	}
}

func TestTargetNativelyServesResponsesAPI(t *testing.T) {
	// The native-passthrough set governs which TARGET formats can
	// receive /v1/responses traffic verbatim (no Responses↔canonical
	// codec). The set is intentionally narrow — only providers whose
	// /v1/responses endpoint has been verified with a captured 200.
	// Drift here is the failure mode the cross-format guard
	// catches; this test pins the current sanctioned set.
	b := testBridge(t)
	if !b.TargetNativelyServesResponsesAPI(provcore.FormatOpenAI) {
		t.Errorf("FormatOpenAI must natively serve /v1/responses (lockstep with spec_openai RequestShapes)")
	}
	// Bedrock must NOT be in the native-passthrough set (uses AWS
	// event-stream framing — no SSE on streams).
	if b.TargetNativelyServesResponsesAPI(provcore.FormatBedrock) {
		t.Errorf("FormatBedrock must NOT serve /v1/responses natively")
	}
}

// refChatRoutable mirrors the production rules in [Bridge.ChatRoutable] so tests
// fail if the two drift apart. Update this helper in the same commit as any
// routing-matrix change.
//
// "OpenAI-like" widened to provcore.Format.IsOpenAIFamily() to cover
// every adapter that uses openai.IdentityCodec (Moonshot, Mistral,
// Groq, Together, Fireworks, Perplexity, XAI, MiniMax, HuggingFace).
// Pre-fix the bridge accepted only 4 of those, so an OpenAI-ingress
// streaming request that auto-routed to e.g. Moonshot was rejected at
// the cross-format streaming gate even though both sides speak the
// exact same wire format.
func refChatRoutable(ingress, target provcore.Format) bool {
	// Embeddings-only formats (Voyage today) have no chat-completions codec
	// — they cannot serve as a chat target, and same-format passthrough is
	// likewise rejected for chat traffic.
	supportsChat := func(f provcore.Format) bool {
		if !f.Valid() {
			return false
		}
		switch f {
		case provcore.FormatVoyage:
			return false
		default:
			return true
		}
	}
	if ingress == target {
		return supportsChat(ingress)
	}
	if !supportsChat(target) {
		return false
	}
	openAILike := func(f provcore.Format) bool {
		return f.IsOpenAIFamily()
	}
	hubIngress := func(f provcore.Format) bool {
		switch f {
		case provcore.FormatAnthropic, provcore.FormatGemini, provcore.FormatVertex:
			return true
		default:
			return false
		}
	}
	if openAILike(ingress) && supportsChat(target) {
		return true
	}
	if openAILike(target) && hubIngress(ingress) {
		return true
	}
	return hubIngress(ingress) && supportsChat(target)
}

func TestChatRoutable_MatchesReferenceMatrix(t *testing.T) {
	b := testBridge(t)
	for _, ingress := range provcore.AllFormats() {
		for _, target := range provcore.AllFormats() {
			got := b.ChatRoutable(ingress, target)
			want := refChatRoutable(ingress, target)
			if got != want {
				t.Errorf("ChatRoutable(%s,%s)=%v ref=%v", ingress, target, got, want)
			}
		}
	}
}

func TestChatRoutable_CountsExpectedCrossEdges(t *testing.T) {
	b := testBridge(t)
	var cross int
	for _, ingress := range provcore.AllFormats() {
		for _, target := range provcore.AllFormats() {
			if ingress == target {
				continue
			}
			if b.ChatRoutable(ingress, target) {
				cross++
			}
		}
	}
	if cross < 40 {
		t.Fatalf("expected many cross-pairs routable, got %d", cross)
	}
}

func TestChatRoutable_BedrockIngressOnlySelf(t *testing.T) {
	b := testBridge(t)
	if b.ChatRoutable(provcore.FormatBedrock, provcore.FormatOpenAI) {
		t.Fatal("bedrock native ingress→openai must stay unroutable until a bedrock ingress→canonical mapper exists")
	}
	if !b.ChatRoutable(provcore.FormatBedrock, provcore.FormatBedrock) {
		t.Fatal("bedrock→bedrock same-format must be routable")
	}
}

// chatKindWireShapesDelegatingToChatRoutable lists the chat-kind WireShapes
// whose EndpointRoutable result MUST equal ChatRoutable. WireShapeOpenAIResponses
// and WireShapeOpenAICompletionsLegacy are chat-kind per KindFromWireShape but
// are intentionally EXCLUDED: they carry dedicated rules (Responses native-
// passthrough gate; legacy /v1/completions OpenAI-only translation) and have
// their own matrix tests above.
var chatKindWireShapesDelegatingToChatRoutable = []typology.WireShape{
	typology.WireShapeOpenAIChat,
	typology.WireShapeAnthropicMessages,
	typology.WireShapeGeminiGenerateContent,
	typology.WireShapeVertexGenerateContent,
	typology.WireShapeBedrockConverse,
	typology.WireShapeBedrockInvoke,
	typology.WireShapeCohereChat,
}

// TestEndpointRoutable_ChatMatrix pins the contract that EVERY chat-kind
// WireShape (not just openai-chat) routes via ChatRoutable. This is the test
// that was missing: before the fix only WireShapeOpenAIChat was wired to
// ChatRoutable and the anthropic-messages / gemini-generate-content shapes fell
// into a same-format-only default — so /v1/messages and /v1beta could only ever
// reach their own provider's models. Dispatching EndpointRoutable by
// KindFromWireShape closes that gap, and this matrix guards against a
// regression where a new chat WireShape silently drops back to same-format.
func TestEndpointRoutable_ChatMatrix(t *testing.T) {
	b := testBridge(t)
	for _, ep := range chatKindWireShapesDelegatingToChatRoutable {
		if typology.KindFromWireShape(ep) != typology.EndpointKindChat {
			t.Fatalf("%s must classify as EndpointKindChat", ep)
		}
		for _, ingress := range provcore.AllFormats() {
			for _, target := range provcore.AllFormats() {
				got := b.EndpointRoutable(ep, ingress, target)
				want := b.ChatRoutable(ingress, target)
				if got != want {
					t.Errorf("EndpointRoutable(%s,%s,%s)=%v want ChatRoutable=%v",
						ep, ingress, target, got, want)
				}
			}
		}
	}
}

// TestEndpointRoutable_CrossProviderIngressUnblocked locks in the exact
// product intent stated by the maintainer: each ingress endpoint accepts only
// its own wire format, but may route to ANY model regardless of the model's
// native provider. It asserts the precise (ingress→target) pairs that the
// production --all-ingress smoke reported as "cannot be routed ... in this
// release" on /v1/messages (anthropic) and /v1beta (gemini) are now routable.
func TestEndpointRoutable_CrossProviderIngressUnblocked(t *testing.T) {
	b := testBridge(t)
	cases := []struct {
		name    string
		ep      typology.WireShape
		ingress provcore.Format
		target  provcore.Format
	}{
		// /v1/messages (anthropic) → non-anthropic models.
		{"messages→openai", typology.WireShapeAnthropicMessages, provcore.FormatAnthropic, provcore.FormatOpenAI},
		{"messages→gemini", typology.WireShapeAnthropicMessages, provcore.FormatAnthropic, provcore.FormatGemini},
		// /v1beta (gemini) → non-gemini models.
		{"gemini→openai", typology.WireShapeGeminiGenerateContent, provcore.FormatGemini, provcore.FormatOpenAI},
		{"gemini→anthropic", typology.WireShapeGeminiGenerateContent, provcore.FormatGemini, provcore.FormatAnthropic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !b.EndpointRoutable(tc.ep, tc.ingress, tc.target) {
				t.Fatalf("EndpointRoutable(%s,%s,%s)=false; cross-provider routing must be allowed",
					tc.ep, tc.ingress, tc.target)
			}
		})
	}

	// Guard the inverse of the bug: the anthropic-messages / gemini ingresses
	// must NOT be same-format-only. Count their cross-provider routable targets.
	for _, ep := range []struct {
		ws      typology.WireShape
		ingress provcore.Format
	}{
		{typology.WireShapeAnthropicMessages, provcore.FormatAnthropic},
		{typology.WireShapeGeminiGenerateContent, provcore.FormatGemini},
	} {
		var cross int
		for _, target := range provcore.AllFormats() {
			if target == ep.ingress {
				continue
			}
			if b.EndpointRoutable(ep.ws, ep.ingress, target) {
				cross++
			}
		}
		if cross == 0 {
			t.Errorf("%s ingress is still same-format-only (0 cross-provider targets) — the gate gap is not closed", ep.ws)
		}
	}
}
