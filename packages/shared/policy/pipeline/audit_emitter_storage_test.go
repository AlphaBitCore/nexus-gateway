package pipeline

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	redactpkg "github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/redact"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Storage-policy governance in buildEvent: the emitter is the single
// choke point through which compliance-proxy and agent audit rows pass,
// so every persisted copy — raw captured body AND normalized sidecar —
// must obey the stage result's StorageAction before the event reaches
// any writer.

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

// chatNormalized builds the marshalled NormalizedPayload the runtime
// normalizer would have produced for a chat body containing the marker.
func chatNormalized(t *testing.T, text string) json.RawMessage {
	t.Helper()
	p := normcore.NormalizedPayload{
		Kind:             normcore.KindAIChat,
		NormalizeVersion: normcore.SchemaVersion,
		Messages: []normcore.Message{
			{Role: normcore.RoleUser, Content: []normcore.ContentBlock{{Type: normcore.ContentText, Text: text}}},
		},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal normalized: %v", err)
	}
	return b
}

// markerSpan addresses the marker inside chatNormalized's single content
// block.
func markerSpan(text string) normcore.TransformSpan {
	start := strings.Index(text, emailMarker)
	return normcore.TransformSpan{
		Action:         normcore.ActionRedact,
		ContentAddress: "messages.0.content.0",
		Start:          start,
		End:            start + len(emailMarker),
		Replacement:    "[EMAIL-REDACTED]",
		SourceID:       "pii-email",
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

func TestBuildEvent_KeepPersistsCapturedCopies(t *testing.T) {
	text := "contact " + emailMarker + " now"
	raw := []byte(`{"messages":[{"content":"` + text + `"}]}`)
	norm := chatNormalized(t, text)
	info := AuditInfo{TransactionID: "tx-keep", RequestNormalized: norm}
	result := &CompliancePipelineResult{Decision: Approve, StorageAction: "keep"}

	evt := emitAndCapture(t, info, result, nil, raw, nil)

	if string(evt.RequestBody.InlineBytes) != string(raw) {
		t.Errorf("keep must persist captured raw bytes, got %q", evt.RequestBody.InlineBytes)
	}
	if string(evt.RequestNormalized) != string(norm) {
		t.Errorf("keep must persist normalized copy unchanged")
	}
	if evt.RequestRedactionSpans != nil {
		t.Errorf("keep emits no redaction spans, got %q", evt.RequestRedactionSpans)
	}
}

func TestBuildEvent_ApproveStorageRedact_RawDroppedWithoutRewriteCopy(t *testing.T) {
	// approve + storageAction=redact with NO inflight rewrite: the raw
	// captured copy has no redacted counterpart, so it must drop to nil —
	// persisting the original would make the audit store the leak.
	text := "contact " + emailMarker + " now"
	raw := []byte(`{"messages":[{"content":"` + text + `"}]}`)
	norm := chatNormalized(t, text)
	info := AuditInfo{TransactionID: "tx-redact", RequestNormalized: norm}
	result := &CompliancePipelineResult{
		Decision:       Approve,
		StorageAction:  "redact",
		TransformSpans: []normcore.TransformSpan{markerSpan(text)},
	}

	evt := emitAndCapture(t, info, result, nil, raw, nil)

	if evt.RequestBody.Kind != audit.BodyAbsent {
		t.Errorf("raw body must be absent under storage-redact without a rewrite copy, got kind=%q bytes=%q", evt.RequestBody.Kind, evt.RequestBody.InlineBytes)
	}
	if strings.Contains(string(evt.RequestNormalized), emailMarker) {
		t.Fatalf("normalized copy leaks the marker: %q", evt.RequestNormalized)
	}
	if !strings.Contains(string(evt.RequestNormalized), "[EMAIL-REDACTED]") {
		t.Errorf("normalized copy must carry the redacted text, got %q", evt.RequestNormalized)
	}
	var spans []normcore.TransformSpan
	if err := json.Unmarshal(evt.RequestRedactionSpans, &spans); err != nil || len(spans) != 1 {
		t.Fatalf("want 1 relocated redaction span, got %q (err %v)", evt.RequestRedactionSpans, err)
	}
	if spans[0].ContentAddress != "messages.0.content.0" {
		t.Errorf("span address = %q, want messages.0.content.0", spans[0].ContentAddress)
	}
}

func TestBuildEvent_StorageRedact_RawKeepsRewrittenCopy(t *testing.T) {
	// Inflight rewrite produced a redacted wire copy: under storage-redact
	// that copy (and only that copy) persists as the raw payload.
	text := "contact " + emailMarker + " now"
	raw := []byte(`{"messages":[{"content":"` + text + `"}]}`)
	redacted := []byte(`{"messages":[{"content":"contact [EMAIL-REDACTED] now"}]}`)
	norm := chatNormalized(t, text)
	info := AuditInfo{
		TransactionID:       "tx-rewrite",
		RequestNormalized:   norm,
		RequestBodyRedacted: redacted,
	}
	result := &CompliancePipelineResult{
		Decision:       Modify,
		StorageAction:  "redact",
		TransformSpans: []normcore.TransformSpan{markerSpan(text)},
	}

	evt := emitAndCapture(t, info, result, nil, raw, nil)

	if string(evt.RequestBody.InlineBytes) != string(redacted) {
		t.Errorf("raw body must be the rewritten copy, got %q", evt.RequestBody.InlineBytes)
	}
	if strings.Contains(string(evt.RequestNormalized), emailMarker) {
		t.Errorf("normalized copy leaks the marker: %q", evt.RequestNormalized)
	}
}

func TestBuildEvent_DropContent_NothingPersists(t *testing.T) {
	text := "contact " + emailMarker + " now"
	raw := []byte(`{"messages":[{"content":"` + text + `"}]}`)
	norm := chatNormalized(t, text)
	info := AuditInfo{TransactionID: "tx-drop", RequestNormalized: norm}
	result := &CompliancePipelineResult{
		Decision:       Approve,
		StorageAction:  "drop-content",
		TransformSpans: []normcore.TransformSpan{markerSpan(text)},
	}

	evt := emitAndCapture(t, info, result, nil, raw, nil)

	if evt.RequestBody.Kind != audit.BodyAbsent {
		t.Errorf("drop-content must not persist raw bytes, got %q", evt.RequestBody.InlineBytes)
	}
	var p normcore.NormalizedPayload
	if err := json.Unmarshal(evt.RequestNormalized, &p); err != nil {
		t.Fatalf("placeholder unmarshal: %v", err)
	}
	if !p.Redacted || len(p.Messages) != 0 {
		t.Errorf("normalized must be the {redacted:true} placeholder, got %q", evt.RequestNormalized)
	}
	if len(p.RuleIDs) != 1 || p.RuleIDs[0] != "pii-email" {
		t.Errorf("placeholder ruleIds = %v, want [pii-email]", p.RuleIDs)
	}
	if strings.Contains(string(evt.RequestNormalized), emailMarker) {
		t.Fatalf("placeholder leaks the marker: %q", evt.RequestNormalized)
	}
}

func TestBuildEvent_BlockStorageRedact_NoSpansDegradesToPlaceholder(t *testing.T) {
	// A keyword-style block carries storageAction=redact but no spans —
	// the normalized copy must degrade to the placeholder, never persist
	// content the policy says cannot be stored verbatim.
	text := "the secret launch codes"
	raw := []byte(`{"messages":[{"content":"` + text + `"}]}`)
	norm := chatNormalized(t, text)
	info := AuditInfo{TransactionID: "tx-block", RequestNormalized: norm}
	result := &CompliancePipelineResult{Decision: RejectHard, StorageAction: "redact"}

	evt := emitAndCapture(t, info, result, nil, raw, nil)

	if evt.RequestBody.Kind != audit.BodyAbsent {
		t.Errorf("block + storage-redact without rewrite must drop raw bytes")
	}
	if strings.Contains(string(evt.RequestNormalized), "secret launch codes") {
		t.Fatalf("normalized copy leaks blocked content: %q", evt.RequestNormalized)
	}
	var p normcore.NormalizedPayload
	if err := json.Unmarshal(evt.RequestNormalized, &p); err != nil || !p.Redacted {
		t.Errorf("want redacted placeholder, got %q (err %v)", evt.RequestNormalized, err)
	}
}

func TestBuildEvent_ResponseStageGovernedIndependently(t *testing.T) {
	// Response hooks can demand a stricter storage policy than the request
	// stage; each stage's copies are governed by its own result.
	reqText := "plain request"
	respText := "reply with " + emailMarker
	reqRaw := []byte(`{"messages":[{"content":"` + reqText + `"}]}`)
	respRaw := []byte(`{"choices":[{"message":{"content":"` + respText + `"}}]}`)
	info := AuditInfo{
		TransactionID:      "tx-dual",
		RequestNormalized:  chatNormalized(t, reqText),
		ResponseNormalized: chatNormalized(t, respText),
	}
	reqResult := &CompliancePipelineResult{Decision: Approve}
	respResult := &CompliancePipelineResult{
		Decision:       Approve,
		StorageAction:  "redact",
		TransformSpans: []normcore.TransformSpan{markerSpan(respText)},
	}

	evt := emitAndCapture(t, info, reqResult, respResult, reqRaw, respRaw)

	if string(evt.RequestBody.InlineBytes) != string(reqRaw) {
		t.Errorf("request stage has no storage policy — captured copy persists")
	}
	if evt.ResponseBody.Kind != audit.BodyAbsent {
		t.Errorf("response raw must drop under storage-redact without rewrite copy, got %q", evt.ResponseBody.InlineBytes)
	}
	if strings.Contains(string(evt.ResponseNormalized), emailMarker) {
		t.Fatalf("response normalized leaks the marker: %q", evt.ResponseNormalized)
	}
	if evt.RequestRedactionSpans != nil {
		t.Errorf("request stage unredacted — no spans expected")
	}
	if evt.ResponseRedactionSpans == nil {
		t.Errorf("response stage redacted — spans must be stamped")
	}
}

func TestBuildEvent_NilResultsLeaveCopiesUntouched(t *testing.T) {
	// Compliance-disabled / fast-path emits carry nil results: bodies and
	// normalized copies flow through unmodified.
	raw := []byte(`{"ok":true}`)
	norm := chatNormalized(t, "hello")
	info := AuditInfo{TransactionID: "tx-nil", RequestNormalized: norm}

	evt := emitAndCapture(t, info, nil, nil, raw, nil)

	if string(evt.RequestBody.InlineBytes) != string(raw) {
		t.Errorf("nil result must keep captured bytes")
	}
	if string(evt.RequestNormalized) != string(norm) {
		t.Errorf("nil result must keep normalized copy")
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
	result := &CompliancePipelineResult{Decision: Modify, StorageAction: "redact"}

	evt := emitAndCapture(t, info, result, nil, nil, nil)

	if evt.RequestBody.Kind != audit.BodyAbsent {
		t.Errorf("capture-disabled request must persist no raw bytes, got %q", evt.RequestBody.InlineBytes)
	}
}

// crossFormatSpan addresses the marker at a hook-time projection index
// that does NOT exist on the storage-time payload (cross-format request:
// the hook-time projection indexed system/tool segments as extra
// messages).
func crossFormatSpan(text string) normcore.TransformSpan {
	s := markerSpan(text)
	s.ContentAddress = "messages.2.content.0"
	return s
}

// emailRedetector is the test stand-in for the pipeline-stamped pattern
// re-scanner (Pipeline.redetector backed by pii-detector patterns).
func emailRedetector(text string, ruleIDs []string) []redactpkg.Match {
	var out []redactpkg.Match
	for _, id := range ruleIDs {
		if id != "pii-email" {
			continue
		}
		if i := strings.Index(text, emailMarker); i >= 0 {
			out = append(out, redactpkg.Match{RuleID: id, Start: i, End: i + len(emailMarker), Replacement: "[EMAIL-REDACTED]"})
		}
	}
	return out
}

func TestBuildEvent_StorageRedact_RedetectRecoversUnresolvedSpans(t *testing.T) {
	// Cross-format shape: the span's hook-time address does not resolve on
	// the storage-time payload. With the stage's Redetect closure on the
	// result, the emitter must store the REDACTED conversation — marker
	// present, PII gone — instead of the drop placeholder.
	text := "contact " + emailMarker + " now"
	raw := []byte(`{"messages":[{"content":"` + text + `"}]}`)
	norm := chatNormalized(t, text)
	info := AuditInfo{TransactionID: "tx-redetect", RequestNormalized: norm}
	result := &CompliancePipelineResult{
		Decision:       Approve,
		StorageAction:  "redact",
		TransformSpans: []normcore.TransformSpan{crossFormatSpan(text)},
		Redetect:       emailRedetector,
	}

	evt := emitAndCapture(t, info, result, nil, raw, nil)

	if strings.Contains(string(evt.RequestNormalized), emailMarker) {
		t.Fatalf("re-detected redaction must remove the PII, got %q", evt.RequestNormalized)
	}
	var p normcore.NormalizedPayload
	if err := json.Unmarshal(evt.RequestNormalized, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Redacted {
		t.Fatalf("re-detection succeeded — want the conversation, not a placeholder: %q", evt.RequestNormalized)
	}
	if len(p.Messages) != 1 || !strings.Contains(p.Messages[0].Content[0].Text, "[EMAIL-REDACTED]") {
		t.Errorf("stored conversation must carry the redaction marker, got %q", evt.RequestNormalized)
	}
	var spans []normcore.TransformSpan
	if err := json.Unmarshal(evt.RequestRedactionSpans, &spans); err != nil || len(spans) != 1 {
		t.Fatalf("want 1 relocated span, got %q (err %v)", evt.RequestRedactionSpans, err)
	}
	if spans[0].ContentAddress != "messages.0.content.0" {
		t.Errorf("span must be relocated to the storage-time address, got %q", spans[0].ContentAddress)
	}
}

func TestBuildEvent_StorageRedact_DegradedRowKeepsDiagnosis(t *testing.T) {
	// No redetector (e.g. the spans came from a remote AI guard): the
	// emitter degrades to the placeholder, but the row must stay
	// diagnosable — reason + cause + failed addresses on the payload, and
	// the original spans preserved on the spans column.
	text := "contact " + emailMarker + " now"
	raw := []byte(`{"messages":[{"content":"` + text + `"}]}`)
	norm := chatNormalized(t, text)
	info := AuditInfo{TransactionID: "tx-degraded", RequestNormalized: norm}
	result := &CompliancePipelineResult{
		Decision:       Approve,
		StorageAction:  "redact",
		TransformSpans: []normcore.TransformSpan{crossFormatSpan(text)},
	}

	evt := emitAndCapture(t, info, result, nil, raw, nil)

	if strings.Contains(string(evt.RequestNormalized), emailMarker) {
		t.Fatalf("degraded row must not leak content, got %q", evt.RequestNormalized)
	}
	var p normcore.NormalizedPayload
	if err := json.Unmarshal(evt.RequestNormalized, &p); err != nil {
		t.Fatalf("placeholder unmarshal: %v", err)
	}
	if !p.Redacted || p.RedactedReason != normcore.RedactedReasonDegraded {
		t.Errorf("redactedReason = %q, want %q", p.RedactedReason, normcore.RedactedReasonDegraded)
	}
	if p.RedactedDetail == nil || p.RedactedDetail.Cause != normcore.DegradeCauseSpansUnresolved {
		t.Fatalf("redactedDetail = %+v, want cause %q", p.RedactedDetail, normcore.DegradeCauseSpansUnresolved)
	}
	if len(p.RedactedDetail.FailedAddresses) != 1 || p.RedactedDetail.FailedAddresses[0] != "messages.2.content.0" {
		t.Errorf("failedAddresses = %v, want [messages.2.content.0]", p.RedactedDetail.FailedAddresses)
	}
	var spans []normcore.TransformSpan
	if err := json.Unmarshal(evt.RequestRedactionSpans, &spans); err != nil || len(spans) != 1 {
		t.Fatalf("degraded row must preserve the diagnostic spans, got %q (err %v)", evt.RequestRedactionSpans, err)
	}
	if strings.Contains(string(evt.RequestRedactionSpans), emailMarker) {
		t.Fatalf("spans must never carry matched content, got %q", evt.RequestRedactionSpans)
	}
}
