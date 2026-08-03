// hook_extraction_multimodal_test.go — the gateway-local multimodal text
// extraction and its D7 coverage contract.
//
// Named failure modes tested:
//   - image prompt / TTS input reach the hook pipeline as scannable text
//     (previously the shared adapter returned ErrUnknownSchema → nothing
//     was ever scanned on these routes)
//   - D7 conformance: every user-text slot of the supported OpenAI wire
//     schemas is covered by multimodalUserTextPaths; non-text knobs are not
//   - PII in an image prompt fires the redact hook and FAILS CLOSED (403,
//     ReasonRedactInflightUnsupported) — never forwards unredacted
//   - benign multimodal prompts approve and forward
package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

var (
	imagesIngress = Ingress{WireShape: typology.WireShapeOpenAIImages, BodyFormat: provcore.FormatOpenAI}
	ttsIngress    = Ingress{WireShape: typology.WireShapeOpenAIAudioSpeech, BodyFormat: provcore.FormatOpenAI}
)

// newEmptyHookCache builds a HookConfigCache with NO hook configs — the
// hooks-off path (BuildPipeline returns nil).
func newEmptyHookCache(t *testing.T) *compliance.HookConfigCache {
	t.Helper()
	loader := func(_ context.Context) ([]goHooks.HookConfig, error) {
		return nil, nil
	}
	cache := compliance.NewHookConfigCache(loader, builtins.Registry, 0, slog.Default())
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("cache.Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let the first load populate
	return cache
}

// newMetadataOnlyHookCache builds a pipeline with ONLY a metadata hook
// (request-size — ScansContent()==false), no content scanner. Coverage must
// report "none": nothing about the prompt text was inspected.
func newMetadataOnlyHookCache(t *testing.T) *compliance.HookConfigCache {
	t.Helper()
	loader := func(_ context.Context) ([]goHooks.HookConfig, error) {
		return []goHooks.HookConfig{{
			ID:                "size-1",
			ImplementationID:  "request-size-validator",
			Name:              "size",
			Priority:          10,
			Enabled:           true,
			Stage:             "request",
			FailBehavior:      "fail-open",
			TimeoutMs:         1000,
			ApplicableIngress: []string{"ALL"},
			Config:            map[string]any{"maxBytes": 10_000_000},
		}}, nil
	}
	cache := compliance.NewHookConfigCache(loader, builtins.Registry, 0, slog.Default())
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("cache.Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	return cache
}

func TestExtractMultimodalRequestText(t *testing.T) {
	t.Run("image prompt extracted with KindAIImage", func(t *testing.T) {
		body := []byte(`{"model":"dall-e-3","prompt":"a red fox in the snow","n":2,"size":"1024x1024"}`)
		p := extractMultimodalRequestText(typology.EndpointKindImageGeneration, body)
		if p == nil {
			t.Fatal("expected payload, got nil")
		}
		if p.Kind != normcore.KindAIImage {
			t.Errorf("Kind = %s, want %s", p.Kind, normcore.KindAIImage)
		}
		texts := p.TextProjection()
		if len(texts) != 1 || texts[0] != "a red fox in the snow" {
			t.Errorf("TextProjection = %v, want the prompt only", texts)
		}
	})

	t.Run("tts input extracted", func(t *testing.T) {
		body := []byte(`{"model":"tts-1","input":"read this aloud","voice":"alloy"}`)
		p := extractMultimodalRequestText(typology.EndpointKindTTS, body)
		if p == nil {
			t.Fatal("expected payload, got nil")
		}
		texts := p.TextProjection()
		if len(texts) != 1 || texts[0] != "read this aloud" {
			t.Errorf("TextProjection = %v, want the input only", texts)
		}
	})

	t.Run("no user text → nil (nothing to scan)", func(t *testing.T) {
		if p := extractMultimodalRequestText(typology.EndpointKindImageGeneration, []byte(`{"model":"dall-e-3","n":1}`)); p != nil {
			t.Errorf("expected nil for a body with no prompt, got %+v", p)
		}
	})

	t.Run("array-of-strings prompt is scanned (no bypass)", func(t *testing.T) {
		// {"prompt":["pii…"]} must NOT slip past the hook engine — a bare
		// string-type check would let it through unscanned.
		p := extractMultimodalRequestText(typology.EndpointKindImageGeneration,
			[]byte(`{"prompt":["alice@example.com","second line"]}`))
		if p == nil {
			t.Fatal("array prompt must yield scannable text, got nil (bypass!)")
		}
		texts := p.TextProjection()
		if len(texts) != 2 || texts[0] != "alice@example.com" {
			t.Errorf("array prompt not fully extracted: %v", texts)
		}
	})

	t.Run("object-valued slot → nil (not scannable, honest none)", func(t *testing.T) {
		if p := extractMultimodalRequestText(typology.EndpointKindImageGeneration,
			[]byte(`{"prompt":{"nested":"x"}}`)); p != nil {
			t.Errorf("object-valued prompt must not be treated as scanned text, got %+v", p)
		}
	})

	t.Run("non-multimodal kind → nil (adapter path owns it)", func(t *testing.T) {
		if p := extractMultimodalRequestText(typology.EndpointKindChat, []byte(`{"prompt":"x"}`)); p != nil {
			t.Errorf("chat kind must not take the multimodal extractor, got %+v", p)
		}
	})
}

// TestMultimodalUserTextCoverage is the D7 structural gate: it locks the
// supported OpenAI wire schemas field-by-field against
// multimodalUserTextPaths. Every field carrying user-intent text MUST be
// extracted; every non-text knob MUST NOT be. Adding a multimodal route
// whose shape carries user text in a slot not covered here means that text
// silently skips scanning and redaction — extend the table, then this test.
func TestMultimodalUserTextCoverage(t *testing.T) {
	type field struct {
		path     string
		userText bool
	}
	schemas := []struct {
		kind   typology.EndpointKind
		body   string
		fields []field
	}{
		{
			kind: typology.EndpointKindImageGeneration,
			// Full OpenAI images/generations request schema.
			body: `{"model":"dall-e-3","prompt":"USER_TEXT_PROMPT","n":1,"quality":"hd","response_format":"b64_json","size":"1024x1024","style":"vivid","user":"end-user-id"}`,
			fields: []field{
				{"prompt", true},
				{"model", false}, {"n", false}, {"quality", false},
				{"response_format", false}, {"size", false}, {"style", false},
				{"user", false},
			},
		},
		{
			kind: typology.EndpointKindTTS,
			// Full OpenAI audio/speech request schema.
			body: `{"model":"tts-1","input":"USER_TEXT_INPUT","voice":"alloy","response_format":"mp3","speed":1.0}`,
			fields: []field{
				{"input", true},
				{"model", false}, {"voice", false},
				{"response_format", false}, {"speed", false},
			},
		},
	}

	for _, s := range schemas {
		t.Run(string(s.kind), func(t *testing.T) {
			covered := map[string]bool{}
			for _, p := range multimodalUserTextPaths(s.kind) {
				covered[p] = true
			}
			for _, f := range s.fields {
				if f.userText && !covered[f.path] {
					t.Errorf("%s: user-text slot %q is NOT covered by multimodalUserTextPaths — it would skip scanning and redaction", s.kind, f.path)
				}
				if !f.userText && covered[f.path] {
					t.Errorf("%s: non-text knob %q is wrongly treated as user text", s.kind, f.path)
				}
			}
			// And the extraction itself yields exactly the user-text values.
			p := extractMultimodalRequestText(s.kind, []byte(s.body))
			if p == nil {
				t.Fatal("extraction returned nil for a body with user text")
			}
			texts := p.TextProjection()
			var want []string
			for _, f := range s.fields {
				if f.userText {
					want = append(want, f.path)
				}
			}
			if len(texts) != len(want) {
				t.Errorf("extracted %d text segments, want %d (%v)", len(texts), len(want), texts)
			}
			for _, txt := range texts {
				if !strings.HasPrefix(txt, "USER_TEXT_") {
					t.Errorf("extracted non-user-text value %q", txt)
				}
			}
		})
	}
}

// TestRunRequestHooks_ImagePrompt_PIIRedact_FailsClosed is the business
// assertion for the interim generative posture: PII in an image prompt is
// now SCANNED (the extraction exists), and because the OpenAI adapter
// cannot reverse-encode a redacted prompt onto the images wire yet, the
// gateway fails CLOSED — the unredacted prompt never reaches the provider.
func TestRunRequestHooks_ImagePrompt_PIIRedact_FailsClosed(t *testing.T) {
	h := &Handler{deps: &Deps{
		HookConfigCache: newPiiRedactHookCache(t),
		TrafficAdapter:  &openai.Adapter{},
		Logger:          slog.Default(),
	}}
	body := []byte(`{"model":"dall-e-3","prompt":"portrait of alice@example.com as a wizard"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	auditRec := &audit.Record{RequestID: "req-img-pii"}

	rewritten, _, rejected := h.runRequestHooks(req, rec, auditRec, "req-img-pii", body, routingcore.RoutingTarget{}, imagesIngress, nil, slog.Default())
	if !rejected {
		t.Fatalf("PII image prompt must fail closed (redact unsupported on images wire); response=%s", rec.Body.String())
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status=%d want 403", rec.Code)
	}
	if rewritten != nil {
		t.Errorf("rewritten=%q want nil (nothing may be forwarded)", string(rewritten))
	}
	if auditRec.HookReasonCode != goHooks.ReasonRedactInflightUnsupported {
		t.Errorf("HookReasonCode=%q want %q", auditRec.HookReasonCode, goHooks.ReasonRedactInflightUnsupported)
	}
}

// TestRunRequestHooks_MultimodalBenign_Approves: benign prompts and TTS
// inputs pass the same hook pipeline without rejection — scanning is live,
// not a blanket block.
func TestRunRequestHooks_MultimodalBenign_Approves(t *testing.T) {
	cases := []struct {
		name string
		in   Ingress
		path string
		body string
	}{
		{"image benign", imagesIngress, "/v1/images/generations", `{"model":"dall-e-3","prompt":"a watercolor lighthouse at dawn"}`},
		{"tts benign", ttsIngress, "/v1/audio/speech", `{"model":"tts-1","input":"welcome to the show","voice":"alloy"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{deps: &Deps{
				HookConfigCache: newPiiRedactHookCache(t),
				TrafficAdapter:  &openai.Adapter{},
				Logger:          slog.Default(),
			}}
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			auditRec := &audit.Record{RequestID: "req-benign"}

			_, _, rejected := h.runRequestHooks(req, rec, auditRec, "req-benign", []byte(tc.body), routingcore.RoutingTarget{}, tc.in, nil, slog.Default())
			if rejected {
				t.Fatalf("benign multimodal request must not be rejected; status=%d body=%s", rec.Code, rec.Body.String())
			}
			if auditRec.RequestAction != goHooks.ActionApprove {
				t.Errorf("RequestAction=%q want approve", auditRec.RequestAction)
			}
			// Coverage honesty: a configured pipeline scanned the prompt →
			// the row must record prompt-only, never imply output coverage.
			if auditRec.ComplianceCoverage != "prompt-only" {
				t.Errorf("ComplianceCoverage=%q want prompt-only", auditRec.ComplianceCoverage)
			}
		})
	}
}

// TestRunRequestHooks_Multimodal_CoverageStamps locks the coverage enum
// semantics: pipeline configured → "prompt-only"; no pipeline → "none"
// (nothing was scanned — the operator must never assume policy applied);
// chat traffic carries no stamp (no claim, existing behavior).
func TestRunRequestHooks_Multimodal_CoverageStamps(t *testing.T) {
	t.Run("no pipeline → none", func(t *testing.T) {
		h := &Handler{deps: &Deps{
			HookConfigCache: newEmptyHookCache(t),
			TrafficAdapter:  &openai.Adapter{},
			Logger:          slog.Default(),
		}}
		req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"m","prompt":"x"}`))
		auditRec := &audit.Record{RequestID: "req-cov-none"}
		_, _, rejected := h.runRequestHooks(req, httptest.NewRecorder(), auditRec, "req-cov-none", []byte(`{"model":"m","prompt":"x"}`), routingcore.RoutingTarget{}, imagesIngress, nil, slog.Default())
		if rejected {
			t.Fatal("hooks-off multimodal request must pass")
		}
		if auditRec.ComplianceCoverage != "none" {
			t.Errorf("ComplianceCoverage=%q want none (nothing was scanned)", auditRec.ComplianceCoverage)
		}
	})

	t.Run("metadata-only pipeline → none (no content was scanned)", func(t *testing.T) {
		// The anti-false-assurance case: a pipeline of only rate-limit /
		// size / IP hooks scans no prompt text, so coverage must NOT claim
		// prompt-only — that would be the exact misleading badge the field
		// exists to prevent.
		h := &Handler{deps: &Deps{
			HookConfigCache: newMetadataOnlyHookCache(t),
			TrafficAdapter:  &openai.Adapter{},
			Logger:          slog.Default(),
		}}
		body := `{"model":"dall-e-3","prompt":"a benign lighthouse"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
		auditRec := &audit.Record{RequestID: "req-cov-meta"}
		_, _, rejected := h.runRequestHooks(req, httptest.NewRecorder(), auditRec, "req-cov-meta", []byte(body), routingcore.RoutingTarget{}, imagesIngress, nil, slog.Default())
		if rejected {
			t.Fatal("metadata-only pipeline must not reject a benign request")
		}
		if auditRec.ComplianceCoverage != "none" {
			t.Errorf("ComplianceCoverage=%q want none (only metadata hooks ran, no content scanned)", auditRec.ComplianceCoverage)
		}
	})

	t.Run("chat carries no coverage stamp", func(t *testing.T) {
		h := &Handler{deps: &Deps{
			HookConfigCache: newPiiRedactHookCache(t),
			TrafficAdapter:  &openai.Adapter{},
			Logger:          slog.Default(),
		}}
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		auditRec := &audit.Record{RequestID: "req-cov-chat"}
		h.runRequestHooks(req, httptest.NewRecorder(), auditRec, "req-cov-chat", []byte(body), routingcore.RoutingTarget{}, openAIIngress, nil, slog.Default())
		if auditRec.ComplianceCoverage != "" {
			t.Errorf("chat ComplianceCoverage=%q want empty (no claim)", auditRec.ComplianceCoverage)
		}
	})
}
