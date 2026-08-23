package provbuiltins

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/sjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// This file asserts the internal-namespace guarantee against the REAL registered
// adapters, over every wire shape, in BOTH directions, on real HTTP bytes.
//
// Direction matters and the two halves fail differently.
//
// UPSTREAM. A body the gateway sends must not carry the namespace. When it did,
// Cohere answered 422. The strip that finally held sits at the one frame every
// dispatch path funnels through, and the dispatch package has a structural gate
// proving that frame is the only one that reaches the transport. What this file
// adds is breadth: that gate drives a synthetic passthrough codec on one shape,
// and breadth on the real registry is what turns "the funnel strips" into "every
// adapter that exists is stripped".
//
// DOWNSTREAM. A body the gateway returns must not carry it either, and this half
// had no gate at all. The response-side leak was real and shipped: the three
// spec-conversion egress paths are projections, so they drop the namespace for
// free, but the OpenAI-wire egress is the IDENTITY — canonical already is the
// caller's shape — so whatever a codec left in the namespace was handed to the
// client. Four codecs were leaving something there.
//
// The upstream fixtures live in canonicalBodies (one canonical body per wire
// shape, each carrying both internal carriers). The downstream fixtures are
// below: realistic provider responses, none of which contains the word "nexus",
// so anything the assertion finds was written by us.

// shapeFormat maps each wire shape to the adapter format that serves it.
var shapeFormat = map[typology.WireShape]provcore.Format{
	typology.WireShapeOpenAIChat:            provcore.FormatOpenAI,
	typology.WireShapeOpenAIResponses:       provcore.FormatOpenAI,
	typology.WireShapeOpenAIEmbeddings:      provcore.FormatOpenAI,
	typology.WireShapeAnthropicMessages:     provcore.FormatAnthropic,
	typology.WireShapeGeminiGenerateContent: provcore.FormatGemini,
	typology.WireShapeGeminiEmbedContent:    provcore.FormatGemini,
	typology.WireShapeCohereChat:            provcore.FormatCohere,
	typology.WireShapeCohereEmbed:           provcore.FormatCohere,
	typology.WireShapeCohereRerank:          provcore.FormatCohere,
	typology.WireShapeBedrockConverse:       provcore.FormatBedrock,
	typology.WireShapeBedrockEmbeddings:     provcore.FormatBedrock,
	typology.WireShapeVoyageEmbeddings:      provcore.FormatVoyage,
	typology.WireShapeVoyageRerank:          provcore.FormatVoyage,
}

// probeModelID is the vendor model id each shape is driven with. Most adapters
// accept anything; Bedrock dispatches its embedding codec BY model id and
// refuses an unknown one before the wire, which would silently reduce that arm
// to "no bytes to inspect".
var probeModelID = map[typology.WireShape]string{
	typology.WireShapeBedrockEmbeddings: "amazon.titan-embed-text-v2:0",
}

func targetFor(shape typology.WireShape, f provcore.Format, baseURL string) provcore.CallTarget {
	model := "provider-model-id"
	if m, ok := probeModelID[shape]; ok {
		model = m
	}
	return provcore.CallTarget{
		Format:          f,
		BaseURL:         baseURL,
		APIKey:          "test-key",
		ProviderModelID: model,
		MaxOutputTokens: 1024,
		Extras: map[string]string{
			"aws.accessKey": "AKIAPROBE0000000TEST",
			"aws.secretKey": "test-secret",
			"aws.region":    "us-east-1",
		},
	}
}

func assertNoCarrier(t *testing.T, what string, body []byte) {
	t.Helper()
	for _, carrier := range []string{`"nexus"`, `"nexus_thinking"`} {
		if strings.Contains(string(body), carrier) {
			t.Errorf("%s carries %s: %s", what, carrier, body)
		}
	}
}

// TestWire_NoInternalCarrierReachesAnyUpstream drives every registered adapter
// over every wire shape it serves, on both the native and the cross-format leg,
// and reads the bytes off the socket.
func TestWire_NoInternalCarrierReachesAnyUpstream(t *testing.T) {
	adapters := registeredAdapters(t)
	if len(canonicalBodies) != len(shapeFormat) {
		t.Fatalf("canonicalBodies covers %d shapes but shapeFormat maps %d; a shape "+
			"present in one and not the other is a shape this gate skips",
			len(canonicalBodies), len(shapeFormat))
	}

	for shape, body := range canonicalBodies {
		f, ok := shapeFormat[shape]
		if !ok {
			t.Fatalf("shape %s has no adapter format mapping", shape)
		}
		adapter, ok := adapters[f]
		if !ok {
			t.Fatalf("format %s serves shape %s but is not registered", f, shape)
		}
		for _, leg := range []struct {
			name       string
			bodyFormat provcore.Format
		}{
			{"native", f},
			{"cross-format", otherFormat(f)},
		} {
			t.Run(string(shape)+"/"+leg.name, func(t *testing.T) {
				var sent []byte
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					sent, _ = io.ReadAll(r.Body)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":"x","choices":[],"content":[],"data":[],"results":[],"embeddings":[]}`))
				}))
				defer srv.Close()

				// The decode may well reject this stub response; the request bytes
				// were already sent by then and are what this gate reads.
				_, _ = adapter.Execute(context.Background(), provcore.Request{
					WireShape:  shape,
					Body:       []byte(body),
					BodyFormat: leg.bodyFormat,
					Target:     targetFor(shape, f, srv.URL),
				})
				if len(sent) == 0 {
					t.Skipf("adapter refused before the wire; no request bytes to inspect")
				}
				assertNoCarrier(t, "upstream request", sent)
			})
		}
	}
}

// upstreamResponses are realistic provider replies. None contains "nexus", so a
// hit below is something a codec wrote.
var upstreamResponses = map[typology.WireShape]string{
	typology.WireShapeAnthropicMessages: `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":10,"output_tokens":3,"cache_creation_input_tokens":7,"cache_read_input_tokens":2}}`,
	typology.WireShapeOpenAIChat: `{"id":"chatcmpl-1","object":"chat.completion","model":"m","choices":` +
		`[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`,
	typology.WireShapeOpenAIResponses: `{"id":"resp_1","object":"response","model":"m","status":"completed",` +
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],` +
		`"usage":{"input_tokens":10,"output_tokens":3,"total_tokens":13}}`,
	typology.WireShapeGeminiGenerateContent: `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},` +
		`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"totalTokenCount":13}}`,
	typology.WireShapeCohereChat: `{"id":"c1","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},` +
		`"finish_reason":"COMPLETE","usage":{"tokens":{"input_tokens":10,"output_tokens":3}}}`,
	typology.WireShapeBedrockConverse: `{"output":{"message":{"role":"assistant","content":[{"text":"hi"}]}},` +
		`"stopReason":"end_turn","usage":{"inputTokens":10,"outputTokens":3,"totalTokens":13}}`,
	typology.WireShapeOpenAIEmbeddings: `{"object":"list","model":"m","data":[{"object":"embedding","index":0,` +
		`"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":3,"total_tokens":3}}`,
	typology.WireShapeCohereEmbed: `{"id":"e1","embeddings":{"float":[[0.1,0.2]]},` +
		`"meta":{"billed_units":{"input_tokens":3}}}`,
	typology.WireShapeGeminiEmbedContent: `{"embedding":{"values":[0.1,0.2]}}`,
	typology.WireShapeVoyageEmbeddings: `{"object":"list","model":"m","data":[{"object":"embedding","index":0,` +
		`"embedding":[0.1,0.2]}],"usage":{"total_tokens":3}}`,
	typology.WireShapeBedrockEmbeddings: `{"embedding":[0.1,0.2],"inputTextTokenCount":3}`,
	typology.WireShapeCohereRerank: `{"id":"r1","results":[{"index":0,"relevance_score":0.9}],` +
		`"meta":{"billed_units":{"search_units":1}}}`,
	typology.WireShapeVoyageRerank: `{"object":"list","model":"m","data":[{"index":0,"relevance_score":0.9}],` +
		`"usage":{"total_tokens":3}}`,
}

// TestWire_NoInternalCarrierReachesTheClient is the downstream half: no codec's
// DecodeResponse leaves a carrier in the canonical body it produces.
//
// SCOPE, corrected, because the sentence that used to stand here was backwards.
// It said the OpenAI-wire egress "is the strongest place to put it… because
// that egress is the identity". The identity is exactly why nothing can fail
// there: on an OpenAI-family shape the adapter passes the upstream body through,
// so this asserts on a fixture echoed back, and the fixtures contain no carrier
// by construction. Proved by planting a leak in DecodeResponsesResponse — the
// openai-responses arm stayed green while the same leak in Cohere's decode
// reddened precisely.
//
// So what this test covers is the CROSS-FORMAT decoders, and that is worth
// having. The identity shapes are covered by
// [TestWire_TheTerminalStripIsWhatProtectsTheIdentityEgress] below, which drives
// the thing that actually protects them.
func TestWire_NoInternalCarrierReachesTheClient(t *testing.T) {
	adapters := registeredAdapters(t)
	if len(upstreamResponses) != len(shapeFormat) {
		t.Fatalf("upstreamResponses covers %d shapes but shapeFormat maps %d",
			len(upstreamResponses), len(shapeFormat))
	}

	for shape, upstream := range upstreamResponses {
		f := shapeFormat[shape]
		adapter, ok := adapters[f]
		if !ok {
			t.Fatalf("format %s serves shape %s but is not registered", f, shape)
		}
		t.Run(string(shape), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(upstream))
			}))
			defer srv.Close()

			resp, err := adapter.Execute(context.Background(), provcore.Request{
				WireShape:  shape,
				Body:       []byte(canonicalBodies[shape]),
				BodyFormat: provcore.FormatOpenAI,
				Target:     targetFor(shape, f, srv.URL),
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if len(resp.Body) == 0 {
				t.Fatal("no canonical response body; this case proved nothing")
			}
			assertNoCarrier(t, "canonical response", resp.Body)
		})
	}
}

// On an identity egress the adapter cannot be the defence — it forwards what it
// was given — so the defence is the terminal strip the handler applies before
// the client sees anything. That is the guarantee, and it needs a gate of its
// own: the test above can only observe codecs, and on these shapes there is no
// codec between the bytes and the caller.
//
// A carrier is planted in the UPSTREAM response so the chain has something real
// to remove. The adapter forwarding it is expected and asserted, because it is
// the fact that makes the strip load-bearing rather than belt-and-braces.
func TestWire_TheTerminalStripIsWhatProtectsTheIdentityEgress(t *testing.T) {
	adapters := registeredAdapters(t)
	planted := 0

	for shape, upstream := range upstreamResponses {
		f := shapeFormat[shape]
		if !f.IsOpenAIFamily() {
			continue // covered by the codec gate above
		}
		adapter, ok := adapters[f]
		if !ok {
			t.Fatalf("format %s serves shape %s but is not registered", f, shape)
		}
		t.Run(string(shape), func(t *testing.T) {
			leaky, err := sjson.SetRaw(upstream, "nexus.ext.someprovider.key", `"value"`)
			if err != nil {
				t.Fatalf("plant carrier: %v", err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(leaky))
			}))
			defer srv.Close()

			resp, err := adapter.Execute(context.Background(), provcore.Request{
				WireShape:  shape,
				Body:       []byte(canonicalBodies[shape]),
				BodyFormat: provcore.FormatOpenAI,
				Target:     targetFor(shape, f, srv.URL),
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if len(resp.Body) == 0 {
				t.Fatal("no canonical response body; this case proved nothing")
			}
			if !strings.Contains(string(resp.Body), `"nexus"`) {
				t.Skipf("this adapter removed the carrier itself, so the strip is not the "+
					"only defence here and the planted case proves nothing: %s", resp.Body)
			}
			planted++
			if stripped := canonicalext.Strip(resp.Body); strings.Contains(string(stripped), `"nexus"`) {
				t.Errorf("the terminal strip did not remove a carrier the adapter forwarded, "+
					"so it would reach the client: %s", stripped)
			}
		})
	}

	if planted == 0 {
		t.Fatal("no identity-egress shape forwarded the planted carrier, so this gate " +
			"exercised nothing — the same failure the codec gate had")
	}
}

func otherFormat(f provcore.Format) provcore.Format {
	if f == provcore.FormatAnthropic {
		return provcore.FormatOpenAI
	}
	return provcore.FormatAnthropic
}
