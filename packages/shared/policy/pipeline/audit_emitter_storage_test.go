package pipeline

import (
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// Raw-body governance in buildEvent: the emitter is the single choke point
// through which compliance-proxy and agent audit rows pass, so the
// persisted RAW captured body must obey the stage result's Action
// (approve → captured as-is; redact/block → the redacted rewrite copy, or
// nothing when there is none) before the event reaches any writer. The
// normalized projection is never persisted — buildEvent leaves those
// columns nil and every service recomputes the projection at view time.

const emailMarker = "alice.demo@contoso.com"

func storageTestInput() *core.HookInput {
	return &core.HookInput{
		Stage:       "request",
		SourceIP:    "10.0.0.1",
		TargetHost:  "api.example.com",
		Method:      "POST",
		Path:        "/v1/chat/completions",
		IngressType: "COMPLIANCE_PROXY",
	}
}

func emitAndCapture(t *testing.T, info AuditInfo, reqResult, respResult *CompliancePipelineResult, reqBody, respBody []byte) audit.AuditEvent {
	t.Helper()
	w := &captureWriter{}
	e := NewAuditEmitter(w, testEmitterLogger())
	e.EmitDual(storageTestInput(), info, reqResult, respResult, "BUMP_SUCCESS", 200, 12, reqBody, respBody, traffic.UsageMeta{})
	if w.count() != 1 {
		t.Fatalf("want 1 event, got %d", w.count())
	}
	return w.events[0]
}

func TestBuildEvent_ApprovePersistsCapturedRaw(t *testing.T) {
	text := "contact " + emailMarker + " now"
	raw := []byte(`{"messages":[{"content":"` + text + `"}]}`)
	info := AuditInfo{TransactionID: "tx-approve"}
	result := &CompliancePipelineResult{Decision: Approve, Action: core.ActionApprove}

	evt := emitAndCapture(t, info, result, nil, raw, nil)

	if string(evt.RequestBody.InlineBytes) != string(raw) {
		t.Errorf("approve must persist captured raw bytes, got %q", evt.RequestBody.InlineBytes)
	}
	// The normalized projection is never persisted.
	if evt.RequestNormalized != nil {
		t.Errorf("normalized projection must not be persisted, got %q", evt.RequestNormalized)
	}
	if evt.RequestRedactionSpans != nil {
		t.Errorf("redaction spans must not be persisted, got %q", evt.RequestRedactionSpans)
	}
}

func TestBuildEvent_RedactAction_RawDroppedWithoutRewriteCopy(t *testing.T) {
	// redact action with NO inflight rewrite: the raw captured copy has no
	// redacted counterpart, so it must drop to absent — persisting the
	// original would make the audit store the leak.
	text := "contact " + emailMarker + " now"
	raw := []byte(`{"messages":[{"content":"` + text + `"}]}`)
	info := AuditInfo{TransactionID: "tx-redact"}
	result := &CompliancePipelineResult{Decision: Modify, Action: core.ActionRedact}

	evt := emitAndCapture(t, info, result, nil, raw, nil)

	if evt.RequestBody.Kind != audit.BodyAbsent {
		t.Errorf("raw body must be absent under redact without a rewrite copy, got kind=%q bytes=%q", evt.RequestBody.Kind, evt.RequestBody.InlineBytes)
	}
}

func TestBuildEvent_RedactAction_RawKeepsRewrittenCopy(t *testing.T) {
	// Inflight rewrite produced a redacted wire copy: under the redact
	// action that copy (and only that copy) persists as the raw payload.
	text := "contact " + emailMarker + " now"
	raw := []byte(`{"messages":[{"content":"` + text + `"}]}`)
	redacted := []byte(`{"messages":[{"content":"contact [EMAIL-REDACTED] now"}]}`)
	info := AuditInfo{
		TransactionID:       "tx-rewrite",
		RequestBodyRedacted: redacted,
	}
	result := &CompliancePipelineResult{Decision: Modify, Action: core.ActionRedact}

	evt := emitAndCapture(t, info, result, nil, raw, nil)

	if string(evt.RequestBody.InlineBytes) != string(redacted) {
		t.Errorf("raw body must be the rewritten copy, got %q", evt.RequestBody.InlineBytes)
	}
	if strings.Contains(string(evt.RequestBody.InlineBytes), emailMarker) {
		t.Errorf("raw body leaks the marker: %q", evt.RequestBody.InlineBytes)
	}
}

func TestBuildEvent_BlockAction_DropsRawWithoutRewriteCopy(t *testing.T) {
	// A block decision governs the raw body like redact: with no rewrite
	// copy on hand, the raw blocked content must drop to absent.
	text := "contact " + emailMarker + " now"
	raw := []byte(`{"messages":[{"content":"` + text + `"}]}`)
	info := AuditInfo{TransactionID: "tx-block"}
	result := &CompliancePipelineResult{Decision: RejectHard, Action: core.ActionBlock}

	evt := emitAndCapture(t, info, result, nil, raw, nil)

	if evt.RequestBody.Kind != audit.BodyAbsent {
		t.Errorf("block without a rewrite copy must drop raw bytes, got %q", evt.RequestBody.InlineBytes)
	}
}

func TestBuildEvent_ResponseStageGovernedIndependently(t *testing.T) {
	// Response hooks can demand a stricter action than the request stage;
	// each stage's raw copy is governed by its own result.
	reqText := "plain request"
	respText := "reply with " + emailMarker
	reqRaw := []byte(`{"messages":[{"content":"` + reqText + `"}]}`)
	respRaw := []byte(`{"choices":[{"message":{"content":"` + respText + `"}}]}`)
	info := AuditInfo{TransactionID: "tx-dual"}
	reqResult := &CompliancePipelineResult{Decision: Approve, Action: core.ActionApprove}
	respResult := &CompliancePipelineResult{Decision: Modify, Action: core.ActionRedact}

	evt := emitAndCapture(t, info, reqResult, respResult, reqRaw, respRaw)

	if string(evt.RequestBody.InlineBytes) != string(reqRaw) {
		t.Errorf("approve request stage — captured copy persists")
	}
	if evt.ResponseBody.Kind != audit.BodyAbsent {
		t.Errorf("response raw must drop under redact without a rewrite copy, got %q", evt.ResponseBody.InlineBytes)
	}
}

func TestBuildEvent_NilResultsKeepCapturedRaw(t *testing.T) {
	// Compliance-disabled / fast-path emits carry nil results: a stage that
	// never ran carries no redaction demand, so the captured raw body is
	// stored as-is.
	raw := []byte(`{"ok":true}`)
	info := AuditInfo{TransactionID: "tx-nil"}

	evt := emitAndCapture(t, info, nil, nil, raw, nil)

	if string(evt.RequestBody.InlineBytes) != string(raw) {
		t.Errorf("nil result must keep captured bytes, got %q", evt.RequestBody.InlineBytes)
	}
	if evt.RequestNormalized != nil {
		t.Errorf("normalized projection must not be persisted, got %q", evt.RequestNormalized)
	}
}

func TestBuildEvent_CaptureDisabledNeverResurrectsBytes(t *testing.T) {
	// Capture off → captured nil. Even with a redacted rewrite copy on
	// hand, the storage policy must not store bytes the capture config
	// chose not to keep.
	info := AuditInfo{
		TransactionID:       "tx-nocapture",
		RequestBodyRedacted: []byte(`{"messages":[{"content":"[EMAIL-REDACTED]"}]}`),
	}
	result := &CompliancePipelineResult{Decision: Modify, Action: core.ActionRedact}

	evt := emitAndCapture(t, info, result, nil, nil, nil)

	if evt.RequestBody.Kind != audit.BodyAbsent {
		t.Errorf("capture-disabled request must persist no raw bytes, got %q", evt.RequestBody.InlineBytes)
	}
}
