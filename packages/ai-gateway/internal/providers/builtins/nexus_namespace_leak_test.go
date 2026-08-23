package provbuiltins

import (
	"log/slog"
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// nexusExt is the gateway's extension namespace as a caller supplies it. The
// per-provider keys are real ones their codecs consume, so the same fixture
// serves both halves of this file: nothing internal may egress, and the
// extension must still take effect.
const nexusExt = `"nexus":{"ext":{"openai":{"responses":{"instructions":"be brief"}},` +
	`"cohere":{"input_type":"search_document"},"gemini":{"taskType":"RETRIEVAL_QUERY"}}}`

// nexusThinking is the second internal carrier — a message field, not a root
// one, set by the Anthropic ingress so multi-block thinking signatures survive.
const nexusThinking = `,"reasoning_content":"why","nexus_thinking":[{"thinking":"why","signature":"sig-1"}]`

// canonicalBodies is one minimal canonical body per wire shape, each carrying
// both internal carriers. Canonical means the OpenAI shape — that is what
// reaches the cross-format leg, the leg that does not strip before the codec.
var canonicalBodies = map[typology.WireShape]string{
	typology.WireShapeOpenAIChat:            `{"model":"m","messages":[{"role":"assistant","content":"hi"` + nexusThinking + `}],` + nexusExt + `}`,
	typology.WireShapeOpenAIResponses:       `{"model":"m","input":"hi",` + nexusExt + `}`,
	typology.WireShapeAnthropicMessages:     `{"model":"m","messages":[{"role":"assistant","content":"hi"` + nexusThinking + `}],"max_tokens":8,` + nexusExt + `}`,
	typology.WireShapeGeminiGenerateContent: `{"model":"m","messages":[{"role":"user","content":"hi"}],` + nexusExt + `}`,
	typology.WireShapeCohereChat:            `{"model":"m","messages":[{"role":"user","content":"hi"}],` + nexusExt + `}`,
	typology.WireShapeBedrockConverse:       `{"model":"m","messages":[{"role":"assistant","content":"hi"` + nexusThinking + `}],"max_tokens":8,` + nexusExt + `}`,
	typology.WireShapeOpenAIEmbeddings:      `{"model":"m","input":"hi",` + nexusExt + `}`,
	typology.WireShapeCohereEmbed:           `{"model":"m","input":"hi",` + nexusExt + `}`,
	typology.WireShapeGeminiEmbedContent:    `{"model":"m","input":"hi",` + nexusExt + `}`,
	typology.WireShapeBedrockEmbeddings:     `{"model":"m","input":"hi",` + nexusExt + `}`,
	typology.WireShapeVoyageEmbeddings:      `{"model":"m","input":"hi",` + nexusExt + `}`,
	typology.WireShapeCohereRerank:          `{"model":"m","query":"q","documents":["d"],` + nexusExt + `}`,
	typology.WireShapeVoyageRerank:          `{"model":"m","query":"q","documents":["d"],` + nexusExt + `}`,
}

func registeredAdapters(t *testing.T) map[provcore.Format]provcore.Adapter {
	t.Helper()
	reg := provcore.NewRegistry()
	Register(reg, nil, slog.Default())
	reg.Freeze()
	out := make(map[provcore.Format]provcore.Adapter)
	for _, f := range reg.List() {
		a, ok := reg.Get(f)
		if !ok {
			t.Fatalf("registry lists %s but cannot Get it", f)
		}
		out[f] = a
	}
	if len(out) == 0 {
		t.Fatal("no adapters registered; these gates would prove nothing")
	}
	return out
}

// prepareCrossFormat drives the production path — dispatcher leg selection plus
// codec — rather than the codec alone. A DIFFERENT BodyFormat marks the request
// as cross-format, which is the leg under test: the native leg already strips
// before the codec runs.
func prepareCrossFormat(a provcore.Adapter, f provcore.Format, shape typology.WireShape, body string) ([]byte, error) {
	other := provcore.FormatAnthropic
	if f == provcore.FormatAnthropic {
		other = provcore.FormatOpenAI
	}
	out, _, _, err := a.PrepareBody(provcore.Request{
		WireShape:  shape,
		Body:       []byte(body),
		BodyFormat: other,
		Target:     provcore.CallTarget{ProviderModelID: "provider-model-id"},
	})
	return out, err
}

// The leak half of this pair moved to TestEgress_NoInternalCarrierReachesTheTransport
// in the dispatch package, and the move is the point.
//
// It used to live here, driving Adapter.PrepareBody across the registry, and it
// found 18 leaking adapter/shape pairs the day it was written. But PrepareBody is
// a LEG, and the guarantee cannot live on a leg: the cross-format failover leg
// builds its body in canonicalbridge and never calls PrepareBody, so a gate that
// drives PrepareBody certifies the leg it drives and nothing else — which is
// exactly how the leak survived being "fixed" twice. The assertion now runs at the
// transport frame, where every leg funnels, against a passthrough codec (the worst
// case: the shape every OpenAI-family adapter has).
//
// What stays here is the half PrepareBody genuinely owns.

// TestExtensionNamespaceStillReachesTheWire is the other half, and it exists
// because without it the gate above is satisfied by a fix that destroys the
// feature: stripping the namespace BEFORE the codec would also egress nothing,
// while silently disabling `nexus.ext.<provider>.<key>` entirely.
//
// That capability is the sanctioned way a caller reaches a provider-specific
// field the OpenAI canonical shape cannot express (§3a Rule 4) — Cohere's
// input_type, Gemini's taskType. It must survive every change made in the name
// of not leaking, so the two tests are deliberately written as a pair: one says
// the carrier does not egress, the other says its VALUE did.
func TestExtensionNamespaceStillReachesTheWire(t *testing.T) {
	cases := []struct {
		format    provcore.Format
		shape     typology.WireShape
		body      string
		wirePath  string
		wireValue string
	}{
		{
			// nexus.ext.cohere.input_type → Cohere's own input_type field.
			format:    provcore.FormatCohere,
			shape:     typology.WireShapeCohereEmbed,
			body:      `{"model":"m","input":"hi",` + nexusExt + `}`,
			wirePath:  "input_type",
			wireValue: "search_document",
		},
		{
			// nexus.ext.gemini.taskType → Gemini's taskType field.
			format:    provcore.FormatGemini,
			shape:     typology.WireShapeGeminiEmbedContent,
			body:      `{"model":"m","input":"hi",` + nexusExt + `}`,
			wirePath:  "taskType",
			wireValue: "RETRIEVAL_QUERY",
		},
	}

	adapters := registeredAdapters(t)
	for _, tc := range cases {
		adapter, ok := adapters[tc.format]
		if !ok {
			t.Fatalf("%s is not registered; this case cannot run", tc.format)
		}
		out, err := prepareCrossFormat(adapter, tc.format, tc.shape, tc.body)
		if err != nil {
			t.Fatalf("%s / %s: PrepareBody returned %v", tc.format, tc.shape, err)
		}
		got := gjson.GetBytes(out, tc.wirePath)
		if !got.Exists() {
			t.Errorf("%s / %s: nexus.ext value never reached the wire at %q — the extension "+
				"capability is broken, not merely un-leaked; body=%s",
				tc.format, tc.shape, tc.wirePath, truncate(out, 240))
			continue
		}
		if got.String() != tc.wireValue {
			t.Errorf("%s / %s: wire %q = %q, want %q",
				tc.format, tc.shape, tc.wirePath, got.String(), tc.wireValue)
		}
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
