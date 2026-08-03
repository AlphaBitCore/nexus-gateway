package redact

import (
	"github.com/goccy/go-json"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestStorageRawBody(t *testing.T) {
	captured := []byte(`{"messages":[{"content":"leak@example.com"}]}`)
	redacted := []byte(`{"messages":[{"content":"[REDACTED]"}]}`)
	cases := []struct {
		name     string
		captured []byte
		action   decision.Action
		redacted []byte
		want     []byte
	}{
		{"approve keeps captured", captured, decision.ActionApprove, redacted, captured},
		{"redact uses only the redacted copy", captured, decision.ActionRedact, redacted, redacted},
		{"block uses only the redacted copy", captured, decision.ActionBlock, redacted, redacted},
		{"redact with no redacted copy drops the raw body", captured, decision.ActionRedact, nil, nil},
		{"block with no redacted copy drops the raw body", captured, decision.ActionBlock, nil, nil},
		// The empty action means APPROVE, and this case used to assert the
		// opposite ("fails closed" → nil). That earlier reading is the defect
		// itself: decision.Action is a string, every service's no-hooks-ran path
		// builds a result with the decision set and the action unset, and dropping
		// on that is how the compliance proxy came to persist a request body on
		// every bumped row and a response body on none. Unset means "nothing asked
		// for this content to be withheld", not "the policy is unknown" — and
		// capture is gated separately by the payload-capture config, so storing
		// here cannot override a decision not to capture.
		{"empty action means approve — no redaction demand", captured, decision.Action(""), redacted, captured},
		// A non-empty action nobody recognises is a different thing: a producer
		// bug. It still fails closed, because a body must never be persisted under
		// a policy nobody can name.
		{"unknown action fails closed", captured, decision.Action("bogus"), redacted, nil},
		// Capture disabled (or bodyless request): the storage policy must
		// never resurrect bytes the capture config chose not to store.
		{"nil captured stays nil under approve", nil, decision.ActionApprove, redacted, nil},
		{"nil captured stays nil under redact", nil, decision.ActionRedact, redacted, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StorageRawBody(tc.captured, tc.redacted, tc.action)
			if string(got) != string(tc.want) {
				t.Errorf("StorageRawBody(%q) = %q, want %q", tc.action, got, tc.want)
			}
		})
	}
}

func TestMarshalSpans(t *testing.T) {
	if got := MarshalSpans(nil); got != nil {
		t.Errorf("nil spans → nil, got %q", got)
	}
	if got := MarshalSpans([]normalize.TransformSpan{}); got != nil {
		t.Errorf("empty spans → nil, got %q", got)
	}
	spans := []normalize.TransformSpan{
		{Action: normalize.ActionRedact, ContentAddress: "messages.0.content.0", Start: 1, End: 4, Replacement: "[X]", SourceID: "r-1"},
	}
	got := MarshalSpans(spans)
	var decoded []normalize.TransformSpan
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if len(decoded) != 1 || decoded[0].SourceID != "r-1" || decoded[0].End != 4 {
		t.Errorf("round-trip = %+v, want original span", decoded)
	}
}

// failMarshal swaps the marshal seam for one that always errors and
// returns a restore function. Asserts the package-wide fail-safe: a
// marshal failure stores nothing, never the original bytes.
func failMarshal(t *testing.T) {
	t.Helper()
	orig := marshalJSON
	marshalJSON = func(any) ([]byte, error) { return nil, errMarshalBoom{} }
	t.Cleanup(func() { marshalJSON = orig })
}

type errMarshalBoom struct{}

func (errMarshalBoom) Error() string { return "marshal failed" }

func TestMarshalSpans_MarshalFailureYieldsNil(t *testing.T) {
	failMarshal(t)
	spans := []normalize.TransformSpan{{SourceID: "r-1"}}
	if got := MarshalSpans(spans); got != nil {
		t.Errorf("marshal failure must yield nil, got %q", got)
	}
}

func TestCollectRuleIDs(t *testing.T) {
	if got := CollectRuleIDs(nil); got != nil {
		t.Errorf("nil spans → nil, got %v", got)
	}
	spans := []normalize.TransformSpan{
		{SourceID: "pii-email"},
		{SourceID: ""},          // no attribution — skipped
		{SourceID: "pii-email"}, // duplicate — deduped
		{SourceID: "pii-phone"},
	}
	got := CollectRuleIDs(spans)
	if len(got) != 2 || got[0] != "pii-email" || got[1] != "pii-phone" {
		t.Errorf("CollectRuleIDs = %v, want [pii-email pii-phone]", got)
	}
}

// StorageRawBodyChecked's second return is what makes an unnameable action
// reportable instead of a silent NULL column. The plain form cannot tell the two
// apart, which is why every producer now uses the checked one.
func TestStorageRawBodyChecked_ReportsOnlyAnUnnameableAction(t *testing.T) {
	captured := []byte(`{"messages":[{"content":"leak@example.com"}]}`)
	redacted := []byte(`{"messages":[{"content":"[REDACTED]"}]}`)

	cases := []struct {
		name     string
		captured []byte
		action   decision.Action
		want     []byte
		wantOK   bool
	}{
		{"approve", captured, decision.ActionApprove, captured, true},
		{"empty is approve, not a failure", captured, decision.Action(""), captured, true},
		{"redact", captured, decision.ActionRedact, redacted, true},
		{"unnameable action is reported", captured, decision.Action("bogus"), nil, false},
		{"nothing captured is not a policy failure", nil, decision.ActionApprove, nil, true},
		{"nothing captured under an unnameable action still reports", nil, decision.Action("bogus"), nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := StorageRawBodyChecked(tc.captured, redacted, tc.action)
			if string(got) != string(tc.want) {
				t.Errorf("body = %q, want %q", got, tc.want)
			}
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v — the flag exists so a producer bug gets a log line, "+
					"not a NULL body nobody notices", ok, tc.wantOK)
			}
		})
	}
}
