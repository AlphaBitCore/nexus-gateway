package dispatch

// Every adapter that carries per-model wire rules is driven here through the
// real dispatch path with its production spec, to pin two things at once:
// the rules still fire for the models whose wire needs them, and the models
// whose wire does not are untouched (and stay on the surgical path). Each
// adapter's rules ride its codec contract, so dispatch carries zero vendor
// knowledge — the assertion is that a request reaching the codec through the
// real dispatcher comes back coerced (or verbatim) exactly as the contract says.

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/azure"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/deepseek"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/moonshot"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func passthroughReqFor(body []byte, format Format, model string) Request {
	return Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: format,
		Body:       body,
		Target:     CallTarget{ProviderModelID: model},
	}
}

// TestContractAdapters_QuirksLandViaCodec drives each production spec's
// native chat leg end to end: the quirk model's body loses the rejected
// field with the exact legacy report; the inert (volume) model's body is
// preserved verbatim with the model resolved and nothing reported.
func TestContractAdapters_QuirksLandViaCodec(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec AdapterSpec
		// inert is a model the adapter's rules cannot touch.
		inert string
		// rewritten is a model the rules act on, the body field they act
		// on, and the rewrite they report.
		rewritten   string
		quirkField  string
		wantRewrite string
	}{
		{
			name: "openai", spec: openai.NewSpec(nil),
			inert: "gpt-4o", rewritten: "o3-mini",
			quirkField: `"max_tokens":128`, wantRewrite: "max_tokens→max_completion_tokens",
		},
		{
			name: "azure", spec: azure.NewSpec(nil),
			inert: "gpt-4o-deployment", rewritten: "gpt-5.5",
			quirkField: `"max_tokens":128`, wantRewrite: "max_tokens→max_completion_tokens",
		},
		{
			name: "moonshot", spec: moonshot.NewSpec(nil),
			inert: "moonshot-v1-8k", rewritten: "kimi-k2.5",
			quirkField: `"temperature":0.3`, wantRewrite: "temperature→removed",
		},
		{
			name: "deepseek", spec: deepseek.NewSpec(nil),
			inert: "deepseek-chat", rewritten: "deepseek-reasoner",
			quirkField: `"tool_choice":"required"`, wantRewrite: "tool_choice→removed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &specAdapter{spec: tc.spec, log: slog.Default()}

			out, rewrites, _, err := a.prepareBodyFull(passthroughReqFor(chatBody(tc.quirkField), tc.spec.Format, tc.rewritten))
			if err != nil {
				t.Fatalf("PrepareBody: %v", err)
			}
			if strings.Contains(string(out), tc.quirkField) {
				t.Errorf("%s: %s survived into the body the upstream rejects it in: %s", tc.name, tc.quirkField, out)
			}
			found := false
			for _, r := range rewrites {
				if r == tc.wantRewrite {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: rewrites %v missing %q", tc.name, rewrites, tc.wantRewrite)
			}

			out, rewrites, _, err = a.prepareBodyFull(passthroughReqFor(chatBody(tc.quirkField), tc.spec.Format, tc.inert))
			if err != nil {
				t.Fatalf("PrepareBody(inert): %v", err)
			}
			if !strings.Contains(string(out), tc.quirkField) {
				t.Errorf("%s: body field %s was not preserved verbatim for %q: %s", tc.name, tc.quirkField, tc.inert, out)
			}
			if len(rewrites) != 0 {
				t.Errorf("%s: reported rewrites %v for a model with no wire quirk", tc.name, rewrites)
			}
			if !strings.Contains(string(out), `"model":"`+tc.inert+`"`) {
				t.Errorf("%s: model not resolved to the provider id: %s", tc.name, out)
			}
		})
	}
}

// TestGpt54Boundary_ThroughRealDispatch pins the probed gpt-5.4 exemption
// end-to-end on the native leg: temperature survives (probed 200 on both
// wires), the max_tokens rename still lands (probed 400 without it).
func TestGpt54Boundary_ThroughRealDispatch(t *testing.T) {
	a := &specAdapter{spec: openai.NewSpec(nil), log: slog.Default()}
	body := chatBody(`"temperature":0,"max_tokens":128`)
	out, rewrites, _, err := a.prepareBodyFull(passthroughReqFor(body, FormatOpenAI, "gpt-5.4"))
	if err != nil {
		t.Fatalf("PrepareBody: %v", err)
	}
	if !strings.Contains(string(out), `"temperature":0`) {
		t.Errorf("gpt-5.4 accepts temperature (probed 200); it must survive: %s", out)
	}
	if strings.Contains(string(out), `"max_tokens"`) || !strings.Contains(string(out), `"max_completion_tokens":128`) {
		t.Errorf("the max_tokens rename still applies to gpt-5.4: %s", out)
	}
	if len(rewrites) != 1 || rewrites[0] != "max_tokens→max_completion_tokens" {
		t.Errorf("only the rename is reported: %v", rewrites)
	}
}
