// generative_block_test.go — business pins for the generative prompt
// hard-block escalation: an observe-only content match on an image-generation
// prompt escalates to a hard 403 with the GENERATIVE_PROMPT_BLOCKED reason
// code, benign prompts pass, chat / TTS keep their configured posture,
// enforcing matches keep their own attribution (no re-labeling), and the
// redact fail-closed arm is not double-handled.
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
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// newRulepackHookCache serves one rulepack-engine hook with a single
// "forbidden-topic" rule at the given severity and onMatch action. severity
// "warn" (or an "approve" action) makes the match observe-only: the engine
// emits rule:* tags but never a blocking decision — exactly the posture the
// generative escalation must catch.
func newRulepackHookCache(t *testing.T, severity, action string) *compliance.HookConfigCache {
	t.Helper()
	loader := func(_ context.Context) ([]goHooks.HookConfig, error) {
		return []goHooks.HookConfig{{
			ID:                "rp-1",
			ImplementationID:  "rulepack-engine",
			Name:              "safety-pack",
			Priority:          10,
			Enabled:           true,
			Stage:             "request",
			FailBehavior:      "fail-closed",
			TimeoutMs:         1000,
			ApplicableIngress: []string{"ALL"},
			Config: map[string]any{
				"onMatch": map[string]any{"action": action},
				"_rulePackInstalls": []any{map[string]any{
					"installId":   "inst-1",
					"packName":    "safety-default",
					"packVersion": "1.0.0",
					"enabled":     true,
					"rules": []any{map[string]any{
						"ruleId":   "forbidden-topic",
						"category": "safety",
						"severity": severity,
						"pattern":  `contraband`,
						"flags":    "i",
					}},
				}},
			},
		}}, nil
	}
	cache := compliance.NewHookConfigCache(loader, builtins.Registry, 0, slog.Default())
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("cache.Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	return cache
}

// newContentSafetyObserveHookCache serves one content-safety hook bound with
// an approve (observe-only) action: a match emits detector:content-safety
// tags and no decision change.
func newContentSafetyObserveHookCache(t *testing.T) *compliance.HookConfigCache {
	t.Helper()
	loader := func(_ context.Context) ([]goHooks.HookConfig, error) {
		return []goHooks.HookConfig{{
			ID:                "cs-1",
			ImplementationID:  "content-safety",
			Name:              "content-safety",
			Priority:          10,
			Enabled:           true,
			Stage:             "request",
			FailBehavior:      "fail-closed",
			TimeoutMs:         1000,
			ApplicableIngress: []string{"ALL"},
			Config: map[string]any{
				"onMatch": map[string]any{"action": "approve"},
				"patterns": []any{map[string]any{
					"pattern":  `contraband`,
					"category": "illegal_content",
					"severity": "restricted",
				}},
			},
		}}, nil
	}
	cache := compliance.NewHookConfigCache(loader, builtins.Registry, 0, slog.Default())
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("cache.Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	return cache
}

func imagesHandler(cache *compliance.HookConfigCache) *Handler {
	return &Handler{deps: &Deps{
		HookConfigCache: cache,
		TrafficAdapter:  &openai.Adapter{},
		Logger:          slog.Default(),
	}}
}

// TestRunRequestHooks_GenerativePrompt_ObserveMatch_Escalates403 pins AC-1:
// an image-generation prompt matching a content rule is rejected 403 with
// GENERATIVE_PROMPT_BLOCKED even when the hook posture is observe-only —
// both the severity-observe (warn) and the action-observe (approve) paths.
func TestRunRequestHooks_GenerativePrompt_ObserveMatch_Escalates403(t *testing.T) {
	cases := []struct {
		name     string
		severity string
		action   string
	}{
		{"warn severity observes", "warn", "block"},
		{"approve action observes", "hard", "approve"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := imagesHandler(newRulepackHookCache(t, tc.severity, tc.action))
			body := []byte(`{"model":"dall-e-3","prompt":"paint contraband being smuggled"}`)
			req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(string(body)))
			w := httptest.NewRecorder()
			rec := &audit.Record{RequestID: "req-gen"}

			rewritten, _, rejected := h.runRequestHooks(req, w, rec, "req-gen", body, routingcore.RoutingTarget{}, imagesIngress, nil, slog.Default())
			if !rejected {
				t.Fatalf("observe-only content match on an image prompt must escalate to reject; response=%s", w.Body.String())
			}
			if w.Code != http.StatusForbidden {
				t.Errorf("status=%d want 403", w.Code)
			}
			if rewritten != nil {
				t.Errorf("rewritten=%q want nil (rejected, nothing forwarded)", string(rewritten))
			}
			if rec.RequestAction != goHooks.ActionBlock {
				t.Errorf("RequestAction=%q want block", rec.RequestAction)
			}
			if rec.HookReasonCode != reasonGenerativePromptBlocked {
				t.Errorf("HookReasonCode=%q want %q", rec.HookReasonCode, reasonGenerativePromptBlocked)
			}
			// Attribution: the rule match rides on the compliance tags.
			var ruleTag bool
			for _, tag := range rec.ComplianceTags {
				if tag == "rule:forbidden-topic" {
					ruleTag = true
				}
			}
			if !ruleTag {
				t.Errorf("ComplianceTags=%v want rule:forbidden-topic attributed", rec.ComplianceTags)
			}
			// The wire marker names the escalation for the caller (the
			// formatter normalizes the reason to lower-kebab).
			if hk := w.Header().Get("X-Nexus-Hook"); !strings.Contains(hk, "rejected:safety-pack:generative-prompt-blocked") {
				t.Errorf("X-Nexus-Hook=%q want rejected:safety-pack:generative-prompt-blocked", hk)
			}
			// The aggregate decision is stamped as a hard block so every
			// audit consumer that keys on request_hook_decision counts this
			// as blocked — the per-hook trace still holds each hook's real
			// approve/observe vote.
			if rec.HookDecision != string(goHooks.RejectHard) {
				t.Errorf("HookDecision=%q want %q (escalation stamps a hard block)", rec.HookDecision, goHooks.RejectHard)
			}
			var hookApproved bool
			for _, hr := range rec.HooksPipeline {
				if hr.Decision == string(goHooks.Approve) {
					hookApproved = true
				}
			}
			if !hookApproved {
				t.Errorf("per-hook trace=%+v want the observe hook's own APPROVE vote preserved", rec.HooksPipeline)
			}
		})
	}
}

// TestRunRequestHooks_GenerativePrompt_ContentSafetyObserve_Escalates403 pins
// the second AC-1 detector: an observe-only content-safety match (ReasonCode
// CONTENT_SAFETY_VIOLATION, no rule:* tag) also escalates on an image prompt.
func TestRunRequestHooks_GenerativePrompt_ContentSafetyObserve_Escalates403(t *testing.T) {
	h := imagesHandler(newContentSafetyObserveHookCache(t))
	body := []byte(`{"model":"dall-e-3","prompt":"paint contraband being smuggled"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	rec := &audit.Record{RequestID: "req-cs"}

	_, _, rejected := h.runRequestHooks(req, w, rec, "req-cs", body, routingcore.RoutingTarget{}, imagesIngress, nil, slog.Default())
	if !rejected {
		t.Fatalf("observe-only content-safety match on an image prompt must escalate; response=%s", w.Body.String())
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status=%d want 403", w.Code)
	}
	if rec.HookReasonCode != reasonGenerativePromptBlocked {
		t.Errorf("HookReasonCode=%q want %q", rec.HookReasonCode, reasonGenerativePromptBlocked)
	}
}

// TestRunRequestHooks_GenerativePrompt_KeywordObserve_Escalates403 pins that an
// observe-only keyword-filter match (ReasonCode KEYWORD_BLOCKED, no rule:* tag)
// escalates on an image prompt — an illegal-content ban-list protects
// generative prompts exactly like a rule pack does.
func TestRunRequestHooks_GenerativePrompt_KeywordObserve_Escalates403(t *testing.T) {
	loader := func(_ context.Context) ([]goHooks.HookConfig, error) {
		return []goHooks.HookConfig{{
			ID:                "kw-1",
			ImplementationID:  "keyword-filter",
			Name:              "banlist",
			Priority:          10,
			Enabled:           true,
			Stage:             "request",
			FailBehavior:      "fail-open",
			TimeoutMs:         1000,
			ApplicableIngress: []string{"ALL"},
			Config: map[string]any{
				"onMatch":  map[string]any{"action": "approve"},
				"patterns": []any{map[string]any{"pattern": "contraband", "category": "banned"}},
			},
		}}, nil
	}
	cache := compliance.NewHookConfigCache(loader, builtins.Registry, 0, slog.Default())
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("cache.Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	h := imagesHandler(cache)
	body := []byte(`{"model":"dall-e-3","prompt":"paint contraband being smuggled"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	rec := &audit.Record{RequestID: "req-kw"}

	_, _, rejected := h.runRequestHooks(req, w, rec, "req-kw", body, routingcore.RoutingTarget{}, imagesIngress, nil, slog.Default())
	if !rejected {
		t.Fatalf("observe-only keyword match on an image prompt must escalate; response=%s", w.Body.String())
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status=%d want 403", w.Code)
	}
	if rec.HookReasonCode != reasonGenerativePromptBlocked {
		t.Errorf("HookReasonCode=%q want %q", rec.HookReasonCode, reasonGenerativePromptBlocked)
	}
}

// TestRunRequestHooks_TTSPrompt_RedactMatch_FailsClosed pins that a redact
// match on a TTS prompt keeps the P1 fail-closed posture (403
// REDACT_INFLIGHT_UNSUPPORTED — the speech wire cannot carry a redacted input)
// and that TTS is NOT escalated with the generative reason code.
func TestRunRequestHooks_TTSPrompt_RedactMatch_FailsClosed(t *testing.T) {
	h := imagesHandler(newPiiRedactHookCache(t))
	body := []byte(`{"model":"tts-1","input":"read this to alice@example.com","voice":"alloy"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	rec := &audit.Record{RequestID: "req-tts-redact"}

	_, _, rejected := h.runRequestHooks(req, w, rec, "req-tts-redact", body, routingcore.RoutingTarget{}, ttsIngress, nil, slog.Default())
	if !rejected {
		t.Fatalf("redact demand on the un-rewritable TTS wire must fail closed; response=%s", w.Body.String())
	}
	if rec.HookReasonCode != goHooks.ReasonRedactInflightUnsupported {
		t.Errorf("HookReasonCode=%q want %q (P1 fail-closed arm, not the generative escalation)", rec.HookReasonCode, goHooks.ReasonRedactInflightUnsupported)
	}
}

// TestRunRequestHooks_GenerativePrompt_Benign_Passes pins AC-2: the same
// hook config with a non-matching prompt forwards normally — no false block.
func TestRunRequestHooks_GenerativePrompt_Benign_Passes(t *testing.T) {
	h := imagesHandler(newRulepackHookCache(t, "warn", "block"))
	body := []byte(`{"model":"dall-e-3","prompt":"a calm watercolor of a lighthouse"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	rec := &audit.Record{RequestID: "req-benign"}

	_, _, rejected := h.runRequestHooks(req, w, rec, "req-benign", body, routingcore.RoutingTarget{}, imagesIngress, nil, slog.Default())
	if rejected {
		t.Fatalf("benign image prompt must pass; response=%s", w.Body.String())
	}
	if rec.HookReasonCode == reasonGenerativePromptBlocked {
		t.Errorf("HookReasonCode=%q — benign prompt must not carry the escalation code", rec.HookReasonCode)
	}
	if rec.RequestAction != goHooks.ActionApprove {
		t.Errorf("RequestAction=%q want approve", rec.RequestAction)
	}
}

// TestRunRequestHooks_GenerativePrompt_EnforcingMatch_KeepsOwnAttribution
// pins the no-double-handling contract: a match that already blocks (hard
// severity + block action) exits through the standard block arm with the
// rule-pack's own reason code and BlockingRule attribution — the escalation
// never re-labels an enforced block.
func TestRunRequestHooks_GenerativePrompt_EnforcingMatch_KeepsOwnAttribution(t *testing.T) {
	h := imagesHandler(newRulepackHookCache(t, "hard", "block"))
	body := []byte(`{"model":"dall-e-3","prompt":"paint contraband being smuggled"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	rec := &audit.Record{RequestID: "req-enf"}

	_, _, rejected := h.runRequestHooks(req, w, rec, "req-enf", body, routingcore.RoutingTarget{}, imagesIngress, nil, slog.Default())
	if !rejected {
		t.Fatalf("enforcing rulepack match must block; response=%s", w.Body.String())
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status=%d want 403", w.Code)
	}
	if rec.HookReasonCode != "RULEPACK_MATCH" {
		t.Errorf("HookReasonCode=%q want RULEPACK_MATCH (the enforcing arm's own attribution)", rec.HookReasonCode)
	}
	if rec.BlockingRule == nil || rec.BlockingRule.RuleID != "forbidden-topic" {
		t.Errorf("BlockingRule=%+v want forbidden-topic attributed", rec.BlockingRule)
	}
}

// TestRunRequestHooks_ObserveMatch_NonGenerativeKinds_NotEscalated pins AC-3:
// the same observe-only match on chat and TTS keeps the configured posture —
// the escalation is scoped to generative binary-output endpoints only.
func TestRunRequestHooks_ObserveMatch_NonGenerativeKinds_NotEscalated(t *testing.T) {
	cases := []struct {
		name string
		in   Ingress
		path string
		body string
	}{
		{"chat", openAIIngress, "/v1/chat/completions",
			`{"model":"gpt-4o","messages":[{"role":"user","content":"tell me about contraband"}]}`},
		{"tts", ttsIngress, "/v1/audio/speech",
			`{"model":"tts-1","input":"a story about contraband","voice":"alloy"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := imagesHandler(newRulepackHookCache(t, "warn", "block"))
			body := []byte(tc.body)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			rec := &audit.Record{RequestID: "req-" + tc.name}

			_, _, rejected := h.runRequestHooks(req, w, rec, "req-"+tc.name, body, routingcore.RoutingTarget{}, tc.in, nil, slog.Default())
			if rejected {
				t.Fatalf("observe-only match on %s must keep the configured posture (pass); response=%s", tc.name, w.Body.String())
			}
			if rec.HookReasonCode == reasonGenerativePromptBlocked {
				t.Errorf("HookReasonCode=%q — %s must never carry the generative escalation", rec.HookReasonCode, tc.name)
			}
		})
	}
}

// TestRunRequestHooks_GenerativePrompt_PiiObserve_NotEscalated pins the
// deliberate predicate exclusion: an observe-only PII match (compliance:pii
// tags) is a privacy signal, not generative abuse — it must not false-block
// an image workflow.
func TestRunRequestHooks_GenerativePrompt_PiiObserve_NotEscalated(t *testing.T) {
	loader := func(_ context.Context) ([]goHooks.HookConfig, error) {
		return []goHooks.HookConfig{{
			ID:                "pii-obs",
			ImplementationID:  "pii-detector",
			Name:              "pii-detect",
			Priority:          10,
			Enabled:           true,
			Stage:             "request",
			FailBehavior:      "fail-closed",
			TimeoutMs:         1000,
			ApplicableIngress: []string{"ALL"},
			Config: map[string]any{
				"onMatch": map[string]any{"action": "approve"},
				"patternDefinitions": []any{map[string]any{
					"id":          "email",
					"regex":       `[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}`,
					"flags":       "i",
					"replacement": "[REDACTED_EMAIL]",
				}},
			},
		}}, nil
	}
	cache := compliance.NewHookConfigCache(loader, builtins.Registry, 0, slog.Default())
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("cache.Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	h := imagesHandler(cache)
	body := []byte(`{"model":"dall-e-3","prompt":"a portrait for alice@example.com"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	rec := &audit.Record{RequestID: "req-pii-obs"}

	_, _, rejected := h.runRequestHooks(req, w, rec, "req-pii-obs", body, routingcore.RoutingTarget{}, imagesIngress, nil, slog.Default())
	if rejected {
		t.Fatalf("observe-only PII match must not escalate on an image prompt; response=%s", w.Body.String())
	}
	if rec.HookReasonCode == reasonGenerativePromptBlocked {
		t.Errorf("HookReasonCode=%q — PII observe must not trigger the generative escalation", rec.HookReasonCode)
	}
}

// TestRunRequestHooks_GenerativePrompt_RedactMatch_FailsClosedOnce pins AC-4:
// a redact hook matching an image prompt keeps the P1 fail-closed posture —
// 403 with REDACT_INFLIGHT_UNSUPPORTED (the images wire cannot reverse-encode
// a redacted prompt) — and the escalation does not double-handle it.
func TestRunRequestHooks_GenerativePrompt_RedactMatch_FailsClosedOnce(t *testing.T) {
	h := imagesHandler(newPiiRedactHookCache(t))
	body := []byte(`{"model":"dall-e-3","prompt":"a portrait for alice@example.com"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	rec := &audit.Record{RequestID: "req-redact-img"}

	_, _, rejected := h.runRequestHooks(req, w, rec, "req-redact-img", body, routingcore.RoutingTarget{}, imagesIngress, nil, slog.Default())
	if !rejected {
		t.Fatalf("redact demand on the un-rewritable images wire must fail closed; response=%s", w.Body.String())
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status=%d want 403", w.Code)
	}
	if rec.HookReasonCode != goHooks.ReasonRedactInflightUnsupported {
		t.Errorf("HookReasonCode=%q want %q (P1 fail-closed arm, not the escalation)", rec.HookReasonCode, goHooks.ReasonRedactInflightUnsupported)
	}
	if rec.RequestAction != goHooks.ActionBlock {
		t.Errorf("RequestAction=%q want block", rec.RequestAction)
	}
}

// TestGenerativeHardBlockKind pins the endpoint scope of the escalation:
// binary-output generative kinds only (image now, video the day its routes
// land) — never chat, embeddings, TTS, or STT.
func TestGenerativeHardBlockKind(t *testing.T) {
	want := map[typology.EndpointKind]bool{
		typology.EndpointKindImageGeneration: true,
		typology.EndpointKindVideoGeneration: true,
		typology.EndpointKindChat:            false,
		typology.EndpointKindEmbeddings:      false,
		typology.EndpointKindTTS:             false,
		typology.EndpointKindSTT:             false,
		typology.EndpointKindBatch:           false,
		typology.EndpointKindResponses:       false,
		"":                                   false,
	}
	for kind, expect := range want {
		if got := generativeHardBlockKind(kind); got != expect {
			t.Errorf("generativeHardBlockKind(%q)=%v want %v", kind, got, expect)
		}
	}
}

// TestGenerativePromptContentMatch pins the match predicate: rule-pack
// (BlockingRule or rule:* tag), content-safety (CONTENT_SAFETY_VIOLATION), and
// keyword-filter (KEYWORD_BLOCKED) matches count; PII / metadata do not.
func TestGenerativePromptContentMatch(t *testing.T) {
	cases := []struct {
		name     string
		result   *goHooks.CompliancePipelineResult
		wantHook string
		wantHit  bool
	}{
		{"nil result", nil, "", false},
		{"no hooks", &goHooks.CompliancePipelineResult{}, "", false},
		{"blocking rule attributed", &goHooks.CompliancePipelineResult{
			BlockingRule: &goHooks.BlockingRule{RuleID: "r1"},
		}, "generative-prompt-policy", true},
		{"rule tag from observe match", &goHooks.CompliancePipelineResult{
			HookResults: []goHooks.HookResult{{HookName: "safety-pack", Tags: []string{"rulepack:p1", "rule:r1"}}},
		}, "safety-pack", true},
		{"content-safety observe reason code", &goHooks.CompliancePipelineResult{
			HookResults: []goHooks.HookResult{{HookName: "content-safety", ReasonCode: "CONTENT_SAFETY_VIOLATION", Tags: []string{"severity:restricted"}}},
		}, "content-safety", true},
		{"keyword observe reason code", &goHooks.CompliancePipelineResult{
			HookResults: []goHooks.HookResult{{HookName: "banlist", ReasonCode: "KEYWORD_BLOCKED"}},
		}, "banlist", true},
		{"pii tags excluded", &goHooks.CompliancePipelineResult{
			HookResults: []goHooks.HookResult{{HookName: "pii-detect", Tags: []string{"compliance:pii", "severity:confidential"}}},
		}, "", false},
		{"unrelated tags excluded", &goHooks.CompliancePipelineResult{
			HookResults: []goHooks.HookResult{{HookName: "webhook", Tags: []string{"category:custom"}}},
		}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hook, hit := generativePromptContentMatch(tc.result)
			if hit != tc.wantHit || hook != tc.wantHook {
				t.Errorf("generativePromptContentMatch=%q,%v want %q,%v", hook, hit, tc.wantHook, tc.wantHit)
			}
		})
	}
}
