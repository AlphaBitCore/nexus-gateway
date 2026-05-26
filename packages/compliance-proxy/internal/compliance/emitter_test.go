package compliance

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

type captureWriter struct {
	events []audit.AuditEvent
}

func (w *captureWriter) Enqueue(e audit.AuditEvent)    { w.events = append(w.events, e) }
func (w *captureWriter) Flush(_ context.Context) error { return nil }
func (w *captureWriter) Close(_ context.Context) error { return nil }

func TestEmit_PropagatesTraceID(t *testing.T) {
	w := &captureWriter{}
	emitter := NewAuditEmitter(w, slog.New(slog.NewTextHandler(io.Discard, nil)))

	emitter.Emit(
		&core.HookInput{Stage: "request", IngressType: "COMPLIANCE_PROXY"},
		AuditInfo{TransactionID: "tx-1", TraceID: "trace-agent-flow-abc"},
		&CompliancePipelineResult{Decision: "APPROVE"},
		"BUMP_SUCCESS", 200, 42, nil, nil,
		traffic.UsageMeta{},
	)

	if len(w.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(w.events))
	}
	if got := w.events[0].TraceID; got != "trace-agent-flow-abc" {
		t.Errorf("TraceID = %q, want trace-agent-flow-abc", got)
	}
	if got := w.events[0].TransactionID; got != "tx-1" {
		t.Errorf("TransactionID = %q, want tx-1", got)
	}
	// When no detection data is supplied and no usage meta is passed,
	// the emitter defaults usage_extraction_status to "non_llm".
	if got := w.events[0].UsageExtractionStatus; got != "non_llm" {
		t.Errorf("UsageExtractionStatus = %q, want non_llm", got)
	}
}

func TestEmit_E18RequestMetaAndUsage(t *testing.T) {
	w := &captureWriter{}
	emitter := NewAuditEmitter(w, slog.New(slog.NewTextHandler(io.Discard, nil)))

	pt := 100
	ct := 50
	emitter.Emit(
		&core.HookInput{Stage: "request", IngressType: "COMPLIANCE_PROXY"},
		AuditInfo{
			TransactionID: "tx-2",
			TraceID:       "trace-abc",
			RequestMeta: traffic.RequestMeta{
				Provider:          "openai",
				Model:             "gpt-4o-mini",
				ApiKeyClass:       "sk-",
				ApiKeyFingerprint: "abcdef0123456789",
			},
		},
		&CompliancePipelineResult{Decision: "APPROVE"},
		"BUMP_SUCCESS", 200, 42, nil, nil,
		traffic.UsageMeta{
			PromptTokens:     &pt,
			CompletionTokens: &ct,
			Status:           traffic.UsageStatusOK,
		},
	)

	if len(w.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(w.events))
	}
	ev := w.events[0]
	if ev.Provider != "openai" {
		t.Errorf("Provider = %q, want openai", ev.Provider)
	}
	if ev.Model != "gpt-4o-mini" {
		t.Errorf("Model = %q, want gpt-4o-mini", ev.Model)
	}
	if ev.APIKeyClass != "sk-" {
		t.Errorf("APIKeyClass = %q, want sk-", ev.APIKeyClass)
	}
	if ev.APIKeyFingerprint != "abcdef0123456789" {
		t.Errorf("APIKeyFingerprint = %q", ev.APIKeyFingerprint)
	}
	if ev.PromptTokens != 100 || ev.CompletionTokens != 50 || ev.TotalTokens != 150 {
		t.Errorf("tokens = (%d,%d,%d), want (100,50,150)",
			ev.PromptTokens, ev.CompletionTokens, ev.TotalTokens)
	}
	if ev.UsageExtractionStatus != "ok" {
		t.Errorf("UsageExtractionStatus = %q, want ok", ev.UsageExtractionStatus)
	}
}

// TestEmit_BlockingRule verifies that a CompliancePipelineResult carrying
// a rule-pack BlockingRule is serialised onto the AuditEvent so that it
// flows through to the wire message / traffic_event.blocking_rule column.
func TestEmit_BlockingRule(t *testing.T) {
	w := &captureWriter{}
	emitter := NewAuditEmitter(w, slog.New(slog.NewTextHandler(io.Discard, nil)))

	emitter.Emit(
		&core.HookInput{Stage: "request", IngressType: "COMPLIANCE_PROXY"},
		AuditInfo{TransactionID: "tx-br", TraceID: "trace-br"},
		&CompliancePipelineResult{
			Decision:   "REJECT_HARD",
			Reason:     "blocked by rule pack",
			ReasonCode: "RULEPACK_MATCH",
			BlockingRule: &core.BlockingRule{
				Pack:        "content-safety",
				PackVersion: "1.0.0",
				RuleID:      "violence-kill",
			},
		},
		"BUMP_SUCCESS", 403, 42, nil, nil,
		traffic.UsageMeta{},
	)

	if len(w.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(w.events))
	}
	ev := w.events[0]
	if len(ev.RequestBlockingRule) == 0 {
		t.Fatal("AuditEvent.RequestBlockingRule is empty; expected pack attribution bytes")
	}
	var decoded struct {
		Pack        string `json:"pack"`
		PackVersion string `json:"pack_version"`
		RuleID      string `json:"rule_id"`
	}
	if err := json.Unmarshal(ev.RequestBlockingRule, &decoded); err != nil {
		t.Fatalf("unmarshal BlockingRule: %v", err)
	}
	if decoded.Pack != "content-safety" ||
		decoded.PackVersion != "1.0.0" ||
		decoded.RuleID != "violence-kill" {
		t.Errorf("BlockingRule payload = %+v, want (content-safety, 1.0.0, violence-kill)", decoded)
	}
}
