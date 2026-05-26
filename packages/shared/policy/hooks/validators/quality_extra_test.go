package validators

import (
	"strings"
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestQualityChecker_Factory_OnMatchValidationPropagates(t *testing.T) {
	_, err := NewQualityChecker(&HookConfig{Config: map[string]any{
		"onMatch": map[string]any{"inflightAction": "purge-the-cache"},
	}})
	if err == nil {
		t.Fatal("bad onMatch should be rejected")
	}
	if !strings.Contains(err.Error(), "quality-checker") {
		t.Errorf("error should be wrapped: %v", err)
	}
}

func TestQualityChecker_Factory_ConfigOverridesAccepted(t *testing.T) {
	// Verify every config field flows into the constructed hook.
	cfg := &HookConfig{Config: map[string]any{
		"minResponseLength":     float64(50),
		"expectedFinishReasons": []any{"length", "stop"},
		"detectRefusals":        false,
		"refusalPatterns":       []any{"i refuse"},
		"onMatch":               map[string]any{"inflightAction": "block-soft"},
	}}
	h, err := NewQualityChecker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	qc := h.(*QualityChecker)
	if qc.minResponseLength != 50 {
		t.Errorf("minResponseLength: %d want 50", qc.minResponseLength)
	}
	if len(qc.expectedFinishReasons) != 2 || qc.expectedFinishReasons[0] != "length" {
		t.Errorf("expectedFinishReasons: %v", qc.expectedFinishReasons)
	}
	if qc.detectRefusals {
		t.Errorf("detectRefusals: want false")
	}
	if len(qc.refusalPatterns) != 1 || qc.refusalPatterns[0] != "i refuse" {
		t.Errorf("refusalPatterns: %v", qc.refusalPatterns)
	}
	if qc.onMatch.InflightAction != InflightBlockSoft {
		t.Errorf("onMatch.InflightAction: %q want block-soft", qc.onMatch.InflightAction)
	}
}

func TestQualityChecker_Factory_AbsentOnMatchDefaultsToApprove(t *testing.T) {
	// Quality is log-only by tradition: when operator did not write onMatch
	// the inflightAction should be Approve, NOT block-hard.
	h, err := NewQualityChecker(&HookConfig{Config: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	qc := h.(*QualityChecker)
	if qc.onMatch.InflightAction != InflightApprove {
		t.Errorf("absent onMatch: got %q want approve (log-only)", qc.onMatch.InflightAction)
	}
}

func TestQualityChecker_Execute_ShortResponseLogOnly(t *testing.T) {
	// Default onMatch (absent) → approve. Short response → quality signal
	// stamped via reasonCode but decision stays Approve.
	h, _ := NewQualityChecker(&HookConfig{Config: map[string]any{}})
	res, err := h.Execute(t.Context(), &HookInput{
		Stage:        "response",
		FinishReason: "stop",
		Normalized:   PayloadFromTextSegments([]string{"hi"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != Approve {
		t.Errorf("default log-only: got %s want Approve", res.Decision)
	}
	if res.ReasonCode != "QUALITY_SIGNAL" {
		t.Errorf("ReasonCode: got %q want QUALITY_SIGNAL", res.ReasonCode)
	}
}

func TestQualityChecker_Execute_QualityAnomalyReasonCodeWhenBlocking(t *testing.T) {
	// When operator promotes to block-soft, the ReasonCode flips from
	// QUALITY_SIGNAL to QUALITY_ANOMALY.
	h, _ := NewQualityChecker(&HookConfig{Config: map[string]any{
		"onMatch": map[string]any{"inflightAction": "block-soft"},
	}})
	res, _ := h.Execute(t.Context(), &HookInput{
		Stage:        "response",
		FinishReason: "stop",
		Normalized:   PayloadFromTextSegments([]string{"x"}),
	})
	if res.ReasonCode != "QUALITY_ANOMALY" {
		t.Errorf("blocking variant: got ReasonCode %q want QUALITY_ANOMALY", res.ReasonCode)
	}
}

func TestQualityChecker_Execute_NoSignalsApproves(t *testing.T) {
	h, _ := NewQualityChecker(&HookConfig{Config: map[string]any{
		"onMatch": map[string]any{"inflightAction": "block-soft"},
	}})
	res, _ := h.Execute(t.Context(), &HookInput{
		Stage:        "response",
		FinishReason: "stop",
		Normalized:   PayloadFromTextSegments([]string{"This is a perfectly normal answer with reasonable length and no refusals."}),
	})
	if res.Decision != Approve {
		t.Errorf("clean response: got %s want Approve", res.Decision)
	}
	if res.ReasonCode != "" {
		t.Errorf("clean response ReasonCode: %q", res.ReasonCode)
	}
}

func TestQualityChecker_Execute_BothTextAndFinishReasonEmptyApproves(t *testing.T) {
	// No text + no finish reason → can't evaluate → approve.
	h, _ := NewQualityChecker(&HookConfig{Config: map[string]any{}})
	res, err := h.Execute(t.Context(), &HookInput{Stage: "response"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != Approve {
		t.Errorf("empty input: got %s want Approve", res.Decision)
	}
}

func TestQualityChecker_Execute_AbsorbsFinishReasonFromNormalized(t *testing.T) {
	// HookInput.FinishReason is empty; the hook should pick it up from the
	// payload's assistant Message.FinishReason.
	payload := &normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Messages: []normalize.Message{{
			Role:         normalize.RoleAssistant,
			FinishReason: "content_filter",
			Content: []normalize.ContentBlock{
				{Type: normalize.ContentText, Text: "This response is long enough to pass the min check."},
			},
		}},
	}
	h, _ := NewQualityChecker(&HookConfig{Config: map[string]any{
		"onMatch": map[string]any{"inflightAction": "block-soft"},
	}})
	res, err := h.Execute(t.Context(), &HookInput{Stage: "response", Normalized: payload})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != BlockSoft {
		t.Errorf("absorbed finishReason should trigger signal; got %s", res.Decision)
	}
	if !strings.Contains(res.Reason, "content_filter") {
		t.Errorf("reason should mention content_filter: %q", res.Reason)
	}
}

func TestQualityChecker_Execute_AbsorbsFinishReasonFromPayloadTopLevel(t *testing.T) {
	// FinishReason not on any message but on top-level NormalizedPayload.
	payload := &normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		FinishReason:     "length",
		Messages: []normalize.Message{{
			Role: normalize.RoleAssistant,
			Content: []normalize.ContentBlock{
				{Type: normalize.ContentText, Text: "Output truncated due to length limit reached during generation."},
			},
		}},
	}
	h, _ := NewQualityChecker(&HookConfig{Config: map[string]any{
		"onMatch": map[string]any{"inflightAction": "block-soft"},
	}})
	res, _ := h.Execute(t.Context(), &HookInput{Stage: "response", Normalized: payload})
	if !strings.Contains(res.Reason, "length") {
		t.Errorf("reason should mention top-level finishReason 'length': %q", res.Reason)
	}
}

func TestQualityChecker_Execute_FallsBackToTextProjectionWhenNoAssistantText(t *testing.T) {
	// No assistant message; text-projection fallback should pick up the
	// user-role text.
	payload := PayloadFromTextSegments([]string{"only user-role text but long enough to pass minimum"})
	h, _ := NewQualityChecker(&HookConfig{Config: map[string]any{
		"onMatch": map[string]any{"inflightAction": "block-soft"},
	}})
	res, _ := h.Execute(t.Context(), &HookInput{
		Stage:        "response",
		FinishReason: "stop", // expected → no anomaly
		Normalized:   payload,
	})
	if res.Decision != Approve {
		t.Errorf("user-text fallback length OK + finish ok: got %s want Approve", res.Decision)
	}
}

func TestQualityChecker_Execute_RefusalDetectionDisabled(t *testing.T) {
	// detectRefusals=false → refusal text should NOT trigger signal.
	h, _ := NewQualityChecker(&HookConfig{Config: map[string]any{
		"detectRefusals": false,
		"onMatch":        map[string]any{"inflightAction": "block-soft"},
	}})
	res, _ := h.Execute(t.Context(), &HookInput{
		Stage:        "response",
		FinishReason: "stop",
		Normalized: PayloadFromTextSegments([]string{
			"As an AI I can't help with that — but this is long enough to clear the min.",
		}),
	})
	if res.Decision != Approve {
		t.Errorf("detectRefusals=false: got %s want Approve", res.Decision)
	}
}

func TestQualityChecker_Execute_CustomRefusalPatterns(t *testing.T) {
	// Operator can replace default refusal phrases.
	h, _ := NewQualityChecker(&HookConfig{Config: map[string]any{
		"refusalPatterns": []any{"sorry dave"},
		"onMatch":         map[string]any{"inflightAction": "block-soft"},
	}})
	// Built-in "as an ai" should NOT match anymore (overridden).
	res, _ := h.Execute(t.Context(), &HookInput{
		Stage:        "response",
		FinishReason: "stop",
		Normalized: PayloadFromTextSegments([]string{
			"as an AI I won't but this is long enough to pass min check threshold.",
		}),
	})
	if res.Decision != Approve {
		t.Errorf("overridden patterns must skip built-in phrase; got %s", res.Decision)
	}

	// Custom pattern fires when the configured phrase appears.
	res, _ = h.Execute(t.Context(), &HookInput{
		Stage:        "response",
		FinishReason: "stop",
		Normalized:   PayloadFromTextSegments([]string{"Sorry Dave, I can't do that this time today."}),
	})
	if res.Decision != BlockSoft {
		t.Errorf("custom refusal pattern: got %s want BlockSoft", res.Decision)
	}
}
