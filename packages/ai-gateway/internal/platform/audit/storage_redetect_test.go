package audit

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/redact"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// recordToMessage storage governance: the per-stage Redetect closure
// stamped on the Record must reach redact.ApplyStorageAction so a span
// whose hook-time address does not resolve on the storage-time payload
// is re-located instead of degrading the row to the drop placeholder.

const redetectEmail = "alice.demo@contoso.com"

// redetectNormalizer fakes the shared/normalize closure: it emits a
// single-message chat payload containing the email, regardless of the
// raw bytes — the storage-time projection that disagrees with the
// hook-time span addresses.
func redetectNormalizer(t *testing.T) NormalizeFn {
	t.Helper()
	p := normcore.NormalizedPayload{
		Kind:             normcore.KindAIChat,
		NormalizeVersion: normcore.SchemaVersion,
		Messages: []normcore.Message{
			{Role: normcore.RoleUser, Content: []normcore.ContentBlock{{Type: normcore.ContentText, Text: "mail " + redetectEmail + " now"}}},
		},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return func(direction, contentType, adapterType, model, path string, stream bool, body []byte) (json.RawMessage, string, string) {
		return b, "ok", ""
	}
}

// crossFormatRecord builds a Record whose request span addresses a
// message index that exists only on the hook-time projection.
func crossFormatRecord() *Record {
	return &Record{
		RequestID:            "req-redetect",
		Timestamp:            time.Now(),
		RequestBody:          []byte(`{"messages":[{"content":"mail ` + redetectEmail + ` now"}]}`),
		RequestStorageAction: "redact",
		RequestTransformSpans: []normcore.TransformSpan{
			{Source: normcore.SourceHook, SourceID: "email", Action: normcore.ActionRedact, ContentAddress: "messages.2.content.0", Start: 5, End: 5 + len(redetectEmail), Replacement: "[EMAIL-REDACTED]"},
		},
		RequestRedactRuleIDs: []string{"email"},
	}
}

func TestRecordToMessage_RequestRedetectRecoversUnresolvedSpans(t *testing.T) {
	w := NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default()).WithNormalizer(redetectNormalizer(t))
	rec := crossFormatRecord()
	rec.RequestRedetect = func(text string, ruleIDs []string) []redact.Match {
		for _, id := range ruleIDs {
			if id != "email" {
				continue
			}
			if i := strings.Index(text, redetectEmail); i >= 0 {
				return []redact.Match{{RuleID: id, Start: i, End: i + len(redetectEmail), Replacement: "[EMAIL-REDACTED]"}}
			}
		}
		return nil
	}

	msg := w.recordToMessage(rec)

	if strings.Contains(string(msg.RequestNormalized), redetectEmail) {
		t.Fatalf("stored normalized copy must not leak the email, got %q", msg.RequestNormalized)
	}
	var p normcore.NormalizedPayload
	if err := json.Unmarshal(msg.RequestNormalized, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Redacted {
		t.Fatalf("re-detection succeeded — want the redacted conversation, not a placeholder: %q", msg.RequestNormalized)
	}
	if len(p.Messages) != 1 || !strings.Contains(p.Messages[0].Content[0].Text, "[EMAIL-REDACTED]") {
		t.Errorf("stored conversation must carry the marker, got %q", msg.RequestNormalized)
	}
	if msg.RequestRedactionSpans == nil {
		t.Error("relocated spans must be stamped for the UI badges")
	}
}

func TestRecordToMessage_DegradedRowKeepsSpansAndDiagnosis(t *testing.T) {
	// No Redetect on the record (e.g. spans from a remote AI guard): the
	// row degrades but stays diagnosable — reason + cause + addresses on
	// the placeholder, original spans preserved on the spans column.
	w := NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default()).WithNormalizer(redetectNormalizer(t))
	rec := crossFormatRecord()

	msg := w.recordToMessage(rec)

	if strings.Contains(string(msg.RequestNormalized), redetectEmail) {
		t.Fatalf("degraded row must not leak the email, got %q", msg.RequestNormalized)
	}
	var p normcore.NormalizedPayload
	if err := json.Unmarshal(msg.RequestNormalized, &p); err != nil {
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
	if err := json.Unmarshal(msg.RequestRedactionSpans, &spans); err != nil || len(spans) != 1 {
		t.Fatalf("degraded row must preserve diagnostic spans, got %q (err %v)", msg.RequestRedactionSpans, err)
	}
	if strings.Contains(string(msg.RequestRedactionSpans), redetectEmail) {
		t.Fatalf("spans must never carry matched content, got %q", msg.RequestRedactionSpans)
	}
}
