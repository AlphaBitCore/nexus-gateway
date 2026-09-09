// Package canonicalbridge — request-direction identity across the canonical waist.
//
// Named failure modes:
//   - a same-format request loses content when it crosses canonical and comes back
//   - the streaming intent does not reach a target that signals it differently
//   - a non-streaming request acquires a streaming intent it never had
package canonicalbridge

import (
	"bytes"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

// TestSameFormatRequestSurvivesTheCanonicalWaist covers the request leg for
// ingress == target, which TestShapeRoundTripIdentity_RichContent skips
// outright (`if a == viaB { continue }`).
//
// IngressChatToWire short-circuits that pair to a carrier strip, so the entry
// point is a passthrough — but the waist is still crossed on the same-format
// path whenever a REQUEST HOOK runs: hooks read the canonical body, and a
// Modify decision rebuilds the upstream request from it. A codec that cannot
// re-encode its own canonicalisation therefore drops content the moment a
// compliance rule rewrites anything, and only then, which is the worst time to
// discover it.
//
// The comparison runs on canonical signatures rather than raw bytes: a codec may
// reorder keys or normalise a scalar and still be faithful. What must not change
// is the conversation — its roles, its text, its media parts, its tool
// declarations.
func TestSameFormatRequestSurvivesTheCanonicalWaist(t *testing.T) {
	b := testBridge(t)
	for _, shape := range []provcore.Format{
		provcore.FormatOpenAI,
		provcore.FormatAnthropic,
		provcore.FormatGemini,
	} {
		for _, streaming := range []bool{false, true} {
			name := string(shape)
			if streaming {
				name += "_streaming"
			}
			t.Run(name, func(t *testing.T) {
				native := []byte(richNativeChatBody(shape))
				canonical, err := b.IngressChatToCanonical(shape, native, dummyCallTarget(shape))
				if err != nil {
					t.Fatalf("canonicalize: %v", err)
				}
				if streaming {
					canonical = EnsureCanonicalStream(canonical)
				}
				codec, ok := b.codecs[shape]
				if !ok || codec == nil {
					t.Fatalf("no codec registered for %s", shape)
				}
				enc, err := codec.EncodeRequest(chatWireShapeForFormat(shape), canonical, dummyCallTarget(shape))
				if err != nil {
					t.Fatalf("encode back to %s: %v", shape, err)
				}
				rebuilt, err := b.IngressChatToCanonical(shape, enc.Body, dummyCallTarget(shape))
				if err != nil {
					t.Fatalf("canonicalize the re-encoded body: %v", err)
				}

				before, after := richChatSignature(canonical), richChatSignature(rebuilt)
				if before != after {
					t.Errorf("%s lost content crossing its own canonical waist\n before:\n%s\n after:\n%s\n wire = %s",
						shape, before, after, enc.Body)
				}
			})
		}
	}
}

// TestCohereRequestKeepsEveryFieldItDeclares holds Cohere to the bar its own
// design sets, which is not identity.
//
// The Cohere encoder is a PROJECTION: it declares the field set it may emit
// (codec_request_fields.go) and drops everything else, because Cohere v2 answers
// 422 for any name it does not know, so forwarding the canonical body minus a
// list of known offenders ships a 422 for the first canonical field nobody has
// hit yet. Asserting identity here would assert against a documented decision.
//
// What IS assertable: the conversation itself must survive. A projection that
// drops messages or tool declarations is not implementing that decision, it is
// losing data under cover of it.
// TestOpaqueContinuationTokensSurviveTheirOwnWire asserts on the WIRE BYTES,
// which is the only place this class of loss is visible.
//
// A provider-native continuation token — Gemini's thoughtSignature, and the
// same idea as the Responses reasoning blob — is opaque to the client, cannot
// be regenerated, and is what lets the model continue the chain it started.
// Losing it returns a 200 from a model that has forgotten how it got here.
//
// Comparing canonical signatures cannot see it: if the token were dropped
// during canonicalisation, both sides of that comparison would lack it equally
// and the test would read green. The same-format lane cannot see it either,
// because it forwards raw bytes — so the assertion has to be on what the CODEC
// builds from canonical, which is the body a hook Modify decision sends.
//
// Losing it on the OpenAI or Anthropic wire is correct and not asserted here:
// neither has a field for a Gemini token, and failover re-encodes from
// canonical rather than from the other provider's wire, so a Gemini failover
// target still gets it.
func TestOpaqueContinuationTokensSurviveTheirOwnWire(t *testing.T) {
	b := testBridge(t)
	native := richNativeChatBody(provcore.FormatGemini)

	var token string
	gjson.Get(native, "contents").ForEach(func(_, c gjson.Result) bool {
		c.Get("parts").ForEach(func(_, p gjson.Result) bool {
			if s := p.Get("thoughtSignature").String(); s != "" {
				token = s
			}
			return token == ""
		})
		return token == ""
	})
	if token == "" {
		t.Fatal("the Gemini capture carries no thoughtSignature — re-capture it, or this test " +
			"asserts nothing about the field it names")
	}

	canonical, err := b.IngressChatToCanonical(provcore.FormatGemini, []byte(native),
		dummyCallTarget(provcore.FormatGemini))
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if !bytes.Contains(canonical, []byte(token)) {
		t.Fatalf("the thoughtSignature did not reach canonical, so nothing downstream can put it "+
			"back: %s", canonical)
	}

	codec, ok := b.codecs[provcore.FormatGemini]
	if !ok || codec == nil {
		t.Fatal("no Gemini codec registered")
	}
	enc, err := codec.EncodeRequest(chatWireShapeForFormat(provcore.FormatGemini), canonical,
		dummyCallTarget(provcore.FormatGemini))
	if err != nil {
		t.Fatalf("re-encode to the Gemini wire: %v", err)
	}
	if !bytes.Contains(enc.Body, []byte(token)) {
		t.Errorf("the thoughtSignature is absent from the rebuilt Gemini request — a hook that "+
			"modifies this conversation hands the model a turn missing the chain it was "+
			"continuing: %s", enc.Body)
	}
}

// TestAnthropicThinkingSignatureSurvivesItsOwnWire is the same assertion for
// the other wire that signs its reasoning.
//
// Anthropic signs each thinking block, and a multi-turn conversation must send
// the block back with its signature intact: an extended-thinking turn whose
// tool_use is not preceded by the original signed block is rejected. So the
// signature is not decoration, it is the token that makes the next request
// legal — and like Gemini's, a canonical-signature comparison cannot see it go
// missing.
func TestAnthropicThinkingSignatureSurvivesItsOwnWire(t *testing.T) {
	b := testBridge(t)
	native := requestCorpus(t, "anthropic_thinking_rich_turn")

	var signature string
	gjson.GetBytes(native, "messages").ForEach(func(_, m gjson.Result) bool {
		m.Get("content").ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() == "thinking" {
				signature = block.Get("signature").String()
			}
			return signature == ""
		})
		return signature == ""
	})
	if signature == "" {
		t.Fatal("the Anthropic thinking capture carries no signature — re-capture it with " +
			"thinking enabled, or this test asserts nothing about the field it names")
	}

	canonical, err := b.IngressChatToCanonical(provcore.FormatAnthropic, native,
		dummyCallTarget(provcore.FormatAnthropic))
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if !bytes.Contains(canonical, []byte(signature)) {
		t.Fatalf("the thinking signature did not reach canonical, so nothing downstream can put "+
			"it back: %s", canonical)
	}

	codec, ok := b.codecs[provcore.FormatAnthropic]
	if !ok || codec == nil {
		t.Fatal("no Anthropic codec registered")
	}
	enc, err := codec.EncodeRequest(chatWireShapeForFormat(provcore.FormatAnthropic), canonical,
		dummyCallTarget(provcore.FormatAnthropic))
	if err != nil {
		t.Fatalf("re-encode to the Anthropic wire: %v", err)
	}
	if !bytes.Contains(enc.Body, []byte(signature)) {
		t.Errorf("the thinking signature is absent from the rebuilt Anthropic request — the "+
			"conversation this produces is one Anthropic refuses: %s", enc.Body)
	}
}

func TestCohereRequestKeepsEveryFieldItDeclares(t *testing.T) {
	b := testBridge(t)
	// The media parts come from what the catalog says Cohere chat accepts —
	// file, image and text — in the FORMS this wire takes them. Those are two
	// different facts and conflating them is how the first draft of this test
	// went wrong: the catalog declares the modality, the codec constrains its
	// shape. Cohere reads an image from inline bytes (a data: URL, not an https
	// one) and carries a file as a text passage in documents[] (text/*, not
	// application/pdf). Send either in the other form and the codec refuses,
	// with a message saying why, which is the behaviour to want — and it means
	// the refusal belongs to its own test rather than being smuggled in here by
	// leaving the media out.
	//
	// This body stays hand-built while the other request fixtures moved to real
	// captures, and the reason is a property of Cohere rather than an omission.
	// No single real request satisfies both wires at once:
	//
	//   - OpenAI takes a `file` part as application/pdf; the Cohere codec
	//     refuses a PDF, with the message quoted in its own error — extracting
	//     text from it would change what the model is given.
	//   - Cohere's capabilities are SPLIT across models. Probed 2026-08-29:
	//     command-a-03-2025 answers `400 image content is not supported`, and
	//     command-a-vision-07-2025 answers `400 TOOL_USE_NOT_SUPPORTED`. One
	//     conversation carrying both a tool call and an image is a conversation
	//     Cohere has no model for.
	//
	// So the input here is CANONICAL-side and derived from the codec's stated
	// constraints, not a captured wire — which is what this test measures
	// anyway: the projection, not a round trip. The captured Cohere
	// conversations live beside the others (cohere_rich_turn for the tool half,
	// cohere_vision_turn for the image half).
	declared := chatInputModalities(t, provcore.FormatCohere)
	for _, want := range []string{"image", "file"} {
		if !declared[want] {
			t.Fatalf("the catalog no longer declares %q for Cohere chat — this fixture was derived "+
				"from that declaration, so update both together", want)
		}
	}
	native := `{"model":"cmd","max_tokens":32,
"tools":[{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
"messages":[
 {"role":"system","content":"be terse"},
 {"role":"user","content":[
   {"type":"text","text":"weather?"},
   {"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}},
   {"type":"file","file":{"filename":"notes.txt","file_data":"data:text/plain;base64,aGVsbG8="}}]},
 {"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},
 {"role":"tool","tool_call_id":"c1","content":"18C"},
 {"role":"user","content":"thanks"}]}`
	canonical, err := b.IngressChatToCanonical(provcore.FormatOpenAI,
		[]byte(native), dummyCallTarget(provcore.FormatOpenAI))
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	codec, ok := b.codecs[provcore.FormatCohere]
	if !ok || codec == nil {
		t.Fatal("no Cohere codec registered")
	}
	enc, err := codec.EncodeRequest(chatWireShapeForFormat(provcore.FormatCohere), canonical,
		dummyCallTarget(provcore.FormatCohere))
	if err != nil {
		t.Fatalf("encode to cohere: %v", err)
	}

	if !gjson.GetBytes(enc.Body, "messages").IsArray() {
		t.Fatalf("the Cohere wire carries no messages[]: %s", enc.Body)
	}
	wantTurns := int(gjson.GetBytes(canonical, "messages.#").Int())
	if got := int(gjson.GetBytes(enc.Body, "messages.#").Int()); got != wantTurns {
		t.Errorf("messages: canonical had %d turns, the Cohere wire carries %d — a projection drops "+
			"FIELDS it did not declare, never whole turns", wantTurns, got)
	}
	wantTools := int(gjson.GetBytes(canonical, "tools.#").Int())
	if wantTools > 0 {
		if got := int(gjson.GetBytes(enc.Body, "tools.#").Int()); got != wantTools {
			t.Errorf("tools: canonical declared %d, the Cohere wire carries %d — a dropped declaration "+
				"means the model is never offered the tool", wantTools, got)
		}
	}

	// Counting turns is not enough: a part can vanish while the turn survives,
	// and the count would still match. The image bytes and the document text
	// have to be findable on the wire, wherever this codec chose to put them —
	// inline in the message for the image, lifted into documents[] for the file.
	wire := string(enc.Body)
	if !strings.Contains(wire, "iVBORw0KGgo=") {
		t.Errorf("the image bytes are absent from the Cohere wire — the catalog declares image "+
			"input for these models, so a dropped image is a capability the caller was told "+
			"they had: %s", wire)
	}
	if !strings.Contains(wire, "hello") && !strings.Contains(wire, "aGVsbG8=") {
		t.Errorf("the file's text is absent from the Cohere wire — this codec carries a file as a "+
			"text passage in documents[], so neither the decoded text nor its base64 appearing "+
			"means the document was dropped: %s", wire)
	}
}

// TestStreamingIntentReachesEveryTarget pins the request-direction streaming
// flag, which is the one field whose loss is invisible until a client hangs.
//
// Each wire signals streaming differently: OpenAI and Anthropic carry a body
// field, Gemini switches ENDPOINT (:generateContent → :streamGenerateContent)
// and ignores the body. A canonical body that reaches an Anthropic target
// without `stream` produces a non-streaming upstream call, so the client's SSE
// connection stays open with nothing on it. The reverse matters too: a
// non-streaming request that acquires the flag makes the gateway read an SSE
// body where it expects one JSON object.
func TestStreamingIntentReachesEveryTarget(t *testing.T) {
	b := testBridge(t)
	// Targets whose WIRE carries the intent in the body. Gemini and Vertex are
	// excluded on purpose: their transport picks the streaming endpoint, and the
	// body field is a documented no-op there, so asserting it would pin a
	// behaviour the wire does not have.
	bodyFlagTargets := []provcore.Format{
		provcore.FormatOpenAI,
		provcore.FormatAnthropic,
		provcore.FormatCohere,
	}

	for _, ingress := range []provcore.Format{
		provcore.FormatOpenAI,
		provcore.FormatOpenAIResponses,
		provcore.FormatAnthropic,
		provcore.FormatGemini,
	} {
		for _, target := range bodyFlagTargets {
			if ingress == target || !routableForRequest(b, ingress, target) {
				continue
			}
			t.Run(string(ingress)+"_to_"+string(target), func(t *testing.T) {
				// A MINIMAL body on purpose. This test is about one flag, and the
				// rich fixture carries media some targets refuse by design —
				// Cohere reads images from inline bytes only — so using it here
				// would turn a flag assertion into a media-compatibility one and
				// report a correct rejection as a streaming failure.
				native, err := MinimalNativeChatBody(ingress)
				if err != nil {
					t.Fatalf("minimal body for %s: %v", ingress, err)
				}

				streamed, _, err := b.IngressChatToWire(ingress, target, native, dummyCallTarget(target), true)
				if err != nil {
					t.Fatalf("stream=true: %v", err)
				}
				// A /v1/responses request bound for a target that serves the
				// Responses wire is forwarded VERBATIM, so the streaming intent
				// travels in the client's own body and this function has nothing
				// to stamp. The contract on that branch is that it changes
				// nothing, which is what gets asserted instead — demanding a
				// `stream` field there would be demanding a rewrite of a request
				// the gateway is deliberately not rewriting.
				if ingress == provcore.FormatOpenAIResponses && b.ServesResponses(target, nil, native) {
					if !bytes.Equal(bytes.TrimSpace(streamed), bytes.TrimSpace(native)) {
						t.Errorf("the verbatim Responses lane rewrote the request\n sent: %s\n got:  %s",
							native, streamed)
					}
					return
				}
				if got := gjson.GetBytes(streamed, "stream"); !got.Bool() {
					t.Errorf("stream=true produced a body with stream=%v — the upstream call would "+
						"not stream and the caller's SSE would never receive anything", got.Raw)
				}

				plain, _, err := b.IngressChatToWire(ingress, target, native, dummyCallTarget(target), false)
				if err != nil {
					t.Fatalf("stream=false: %v", err)
				}
				if got := gjson.GetBytes(plain, "stream"); got.Exists() && got.Bool() {
					t.Errorf("stream=false produced a body with stream=true — the gateway would read " +
						"an SSE body where it expects one JSON object")
				}
			})
		}
	}
}

// routableForRequest picks the routability predicate the ingress actually uses.
//
// /v1/responses has its own: ChatRoutable answers false for EVERY target,
// because a Responses request is not a chat request until DecodeResponsesRequest
// turns it into one. Gating on ChatRoutable therefore skipped the whole ingress
// silently — the loop ran, produced no subtests for it, and the suite read
// green. A mounted door going untested is exactly the shape this suite exists
// to catch, so the predicate has to match the door.
func routableForRequest(b *Bridge, ingress, target provcore.Format) bool {
	if ingress == provcore.FormatOpenAIResponses {
		return b.ResponsesRoutable(target)
	}
	return b.ChatRoutable(ingress, target)
}

// TestGeminiStreamingIsAnEndpointNotAField documents the shape the test above
// deliberately does not assert, so the omission reads as a decision rather than
// an oversight.
//
// Gemini's request body has no `stream` field at all; the transport appends
// :streamGenerateContent to the URL. EnsureCanonicalStream still stamps the
// canonical body, and that stamp must not leak into the Gemini wire as a field
// the API does not know.
func TestGeminiStreamingIsAnEndpointNotAField(t *testing.T) {
	b := testBridge(t)
	native := []byte(richNativeChatBody(provcore.FormatOpenAI))
	wire, _, err := b.IngressChatToWire(provcore.FormatOpenAI, provcore.FormatGemini,
		native, dummyCallTarget(provcore.FormatGemini), true)
	if err != nil {
		t.Fatalf("openai→gemini stream=true: %v", err)
	}
	if gjson.GetBytes(wire, "stream").Exists() {
		t.Errorf("the Gemini wire carries a `stream` field: %s\nGemini signals streaming by "+
			"endpoint, and an unknown body field is what its API rejects", wire)
	}
	if !gjson.GetBytes(wire, "contents").Exists() {
		t.Errorf("the Gemini wire lost its contents[]: %s", wire)
	}
}
