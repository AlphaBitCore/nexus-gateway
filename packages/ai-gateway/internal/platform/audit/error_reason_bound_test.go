package audit

import (
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/redact"
)

// The bound belongs to the message, not to the producers.
//
// This is the second attempt at that sentence. The first bounded the producers
// it could find and asserted the field was covered; prod then wrote a 4044-byte
// error_reason on the very first deploy that carried it — the ROUTING_NO_MATCH
// path, whose message is "no available provider for model " + the model string
// the CALLER sent. The caller therefore chose how many bytes landed in the
// audit column, with no ceiling.
//
// Every arm below assigns rec.ErrorReason the way a real producer does and
// asserts the MESSAGE is bounded. A new producer that assigns the field
// directly is covered without knowing this test exists.
func TestRecordToMessage_BoundsErrorReasonWhicheverProducerSetIt(t *testing.T) {
	long := strings.Repeat("X", 4000)

	for _, tc := range []struct {
		name   string
		reason string
	}{
		{
			// stage_routing_passthrough.go -> writeIngressError. The shape that
			// actually escaped: caller-controlled length.
			name:   "gateway routing error echoing a caller-supplied model",
			reason: "no available provider for model nonexistent-" + long,
		},
		{
			// cross_format.go
			name:   "cross-format translation failure",
			reason: "cross-format: " + long,
		},
		{
			// stream_accounting.go — term.err.Error(), arbitrarily long.
			name:   "stream termination carrying a wrapped error",
			reason: "stream aborted: " + long,
		},
		{
			// proxy_upstream.go already bounds, but it must not double-bound
			// into something shorter or differently shaped.
			name:   "an upstream message that is already short",
			reason: "Invalid 'temperature': decimal above maximum value.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &Record{ErrorReason: tc.reason}
			msg := (&Writer{}).recordToMessage(rec)

			if msg.ErrorReason == nil {
				t.Fatalf("a non-empty reason became nil; the audit row loses its explanation")
			}
			got := *msg.ErrorReason
			if len(got) > redact.MaxErrorReasonBytes+3 { // +3 for the "..." marker
				t.Errorf("error_reason is %d bytes; the ceiling is %d. A caller who "+
					"controls this string controls how much they write into every audit row",
					len(got), redact.MaxErrorReasonBytes)
			}
			// The head is what an operator reads, so the bound must keep it.
			head := tc.reason
			if len(head) > 40 {
				head = head[:40]
			}
			if !strings.HasPrefix(got, head) {
				t.Errorf("the bound did not keep the head: got %q, want it to start with %q", got[:min(60, len(got))], head)
			}
		})
	}
}

// An empty reason must stay nil rather than becoming an empty string: the
// column is nullable and "no error" is not the same fact as "an error with no
// message".
func TestRecordToMessage_EmptyErrorReasonStaysNil(t *testing.T) {
	msg := (&Writer{}).recordToMessage(&Record{})
	if msg.ErrorReason != nil {
		t.Errorf("ErrorReason = %q; want nil for a record with no error", *msg.ErrorReason)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
