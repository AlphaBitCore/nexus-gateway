package audit

import (
	"log/slog"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
)

// T5 regression: recordToMessage MUST NOT persist a normalized projection.
// Under the single-action contract the control plane recomputes the
// normalized view at read time from the (already action-governed) raw body,
// so the wire envelope's RequestNormalized / ResponseNormalized and their
// redaction-span columns always stay nil — even when a redact/block action
// fired and a normalizer is wired on the Writer.

const t5Secret = "alice.demo@contoso.com"

// t5LeakyNormalizer returns a closure that, if ever invoked, would emit a
// payload containing the secret. recordToMessage no longer calls it; the
// test fails loudly if a future change re-introduces write-time normalize.
func t5LeakyNormalizer(t *testing.T) NormalizeFn {
	t.Helper()
	return func(direction, contentType, adapterType, model, path string, stream bool, body []byte) (raw json.RawMessage, status, errReason string) {
		t.Fatalf("recordToMessage must never invoke the normalizer (direction=%s); normalized is recomputed at view time", direction)
		return nil, "", ""
	}
}

func TestRecordToMessage_NeverPersistsNormalizedProjection(t *testing.T) {
	captured := []byte(`{"messages":[{"content":"mail ` + t5Secret + ` now"}]}`)
	redacted := []byte(`{"messages":[{"content":"mail [EMAIL-REDACTED] now"}]}`)

	cases := []struct {
		name        string
		reqAction   decision.Action
		respAction  decision.Action
		wantReqBody []byte // expected raw bytes persisted (nil = absent)
	}{
		{"approve persists captured", decision.ActionApprove, decision.ActionApprove, captured},
		{"redact persists redacted copy only", decision.ActionRedact, decision.ActionRedact, redacted},
		{"block persists redacted copy only", decision.ActionBlock, decision.ActionBlock, redacted},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default()).WithNormalizer(t5LeakyNormalizer(t))
			rec := &Record{
				RequestID:            "req-t5",
				Timestamp:            time.Now(),
				RequestBody:          captured,
				RequestBodyRedacted:  redacted,
				RequestAction:        tc.reqAction,
				RequestContentType:   "application/json",
				ResponseBody:         captured,
				ResponseBodyRedacted: redacted,
				ResponseAction:       tc.respAction,
				ResponseContentType:  "application/json",
			}

			msg := w.recordToMessage(rec)

			// The normalized projection is never persisted on either direction.
			if msg.RequestNormalized != nil {
				t.Errorf("RequestNormalized must be nil (view-time recompute), got %q", msg.RequestNormalized)
			}
			if msg.ResponseNormalized != nil {
				t.Errorf("ResponseNormalized must be nil (view-time recompute), got %q", msg.ResponseNormalized)
			}
			if msg.RequestRedactionSpans != nil {
				t.Errorf("RequestRedactionSpans must be nil, got %q", msg.RequestRedactionSpans)
			}
			if msg.ResponseRedactionSpans != nil {
				t.Errorf("ResponseRedactionSpans must be nil, got %q", msg.ResponseRedactionSpans)
			}

			// The RAW body persisted obeys the action: approve = captured,
			// redact/block = the redacted copy. The secret must never leak
			// under a redact/block action.
			if got := msg.RequestBody.InlineBytes; string(got) != string(tc.wantReqBody) {
				t.Errorf("RequestBody = %q, want %q", got, tc.wantReqBody)
			}
			if tc.reqAction != decision.ActionApprove {
				if got := string(msg.RequestBody.InlineBytes); contains(got, t5Secret) {
					t.Errorf("redact/block raw request body leaked the secret: %q", got)
				}
			}
		})
	}
}

// TestStorageRawBody_NoRedactedCopyDropsContent locks the fail-safe: when a
// redact/block action fires but the producer supplied no redacted copy (e.g.
// reverse-encode unsupported), the writer persists nothing rather than the
// unredacted captured bytes.
func TestRecordToMessage_RedactWithoutRedactedCopyDropsRawBody(t *testing.T) {
	captured := []byte(`{"messages":[{"content":"mail ` + t5Secret + ` now"}]}`)
	w := NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default())
	rec := &Record{
		RequestID:          "req-t5-drop",
		Timestamp:          time.Now(),
		RequestBody:        captured,
		RequestAction:      decision.ActionRedact, // no RequestBodyRedacted set
		RequestContentType: "application/json",
	}

	msg := w.recordToMessage(rec)

	if got := msg.RequestBody.InlineBytes; len(got) != 0 {
		t.Errorf("redact without a redacted copy must drop the raw body, got %q", got)
	}
	if msg.RequestNormalized != nil {
		t.Errorf("RequestNormalized must stay nil, got %q", msg.RequestNormalized)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
