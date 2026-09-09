package audit

import (
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
)

// A reason the GATEWAY authored must reach the wire unchanged. This is not
// hypothetical caution: an earlier attempt to gate error_reason here on the
// response body's storage decision destroyed exactly these records.
//
// The premise that attempt rested on — "a gateway-generated reason carries no
// captured response body" — is false. writeIngressError's own header says it
// "ALWAYS stamps the emitted body onto rec.ResponseBody … independent of the
// StoreResponseBody payload gate", and it sets rec.ErrorReason on the same
// record. So a compliance block arrived here with BOTH set, the gate withheld
// the body (action=block, no redacted copy), and the reason was replaced with
// "provider returned HTTP 403" — a claim about a provider that had returned
// 200.
//
// recordToMessage receives a string with no provenance and cannot tell the two
// apart, so it does not try. The permission lives at the producer, where the
// provenance is: redact.ProviderErrorMessage takes allowRawQuote.
func TestRecordToMessage_GatewayAuthoredReasonsSurvive(t *testing.T) {
	// A gateway error envelope: what writeIngressError stamps onto
	// rec.ResponseBody while setting rec.ErrorReason.
	envelope := []byte(`{"error":{"message":"rule-pack match: pii-default/no-ssn (pii)",` +
		`"type":"invalid_request_error","code":"REDACT_FAIL_CLOSED"}}`)

	for _, tc := range []struct {
		name   string
		rec    Record
		reason string
	}{
		{
			// The response hook rejected hard: ResponseAction is block and no
			// redacted copy exists, so the storage gate withholds every byte.
			name: "a compliance block",
			rec: Record{
				StatusCode:     403,
				ErrorCode:      "REDACT_FAIL_CLOSED",
				ErrorReason:    "rule-pack match: pii-default/no-ssn (pii)",
				ResponseBody:   envelope,
				ResponseAction: decision.ActionBlock,
			},
			reason: "rule-pack match: pii-default/no-ssn (pii)",
		},
		{
			// The sharper case: this string exists on NO other column, so
			// replacing it destroys the only record of a gateway-side failure.
			name: "a redaction rewrite failure",
			rec: Record{
				StatusCode:     500,
				ErrorCode:      "RESPONSE_REWRITE_FAILED",
				ErrorReason:    "response rewrite failed",
				ResponseBody:   []byte(`{"error":{"message":"response rewrite failed"}}`),
				ResponseAction: decision.ActionBlock,
			},
			reason: "response rewrite failed",
		},
		{
			name: "a routing refusal",
			rec: Record{
				StatusCode:     404,
				ErrorCode:      "ROUTING_NO_MATCH",
				ErrorReason:    "no available provider for model claude-opus-5",
				ResponseBody:   []byte(`{"error":{"message":"no available provider"}}`),
				ResponseAction: decision.ActionRedact,
			},
			reason: "no available provider for model claude-opus-5",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &Writer{}
			rec := tc.rec
			msg := w.recordToMessage(&rec)

			if msg.ErrorReason == nil {
				t.Fatal("error_reason was dropped — the operator loses the only record of why the gateway refused")
			}
			if *msg.ErrorReason != tc.reason {
				t.Fatalf("the gateway's own reason was rewritten.\n got: %q\nwant: %q\n"+
					"Replacing it with a claim about the provider misdirects the operator triaging this row.",
					*msg.ErrorReason, tc.reason)
			}
			if strings.Contains(*msg.ErrorReason, "provider returned HTTP") {
				t.Fatalf("the reason became a claim about the provider: %q", *msg.ErrorReason)
			}
		})
	}
}

// The bound still applies at this choke point — it is what this layer can
// honestly enforce, and the reason it exists: a gateway-generated
// ROUTING_NO_MATCH was measured on prod at 4044 bytes, thirteen times the cap,
// its size chosen by the caller through the model name.
func TestRecordToMessage_ErrorReasonStaysBounded(t *testing.T) {
	w := &Writer{}
	rec := Record{StatusCode: 404, ErrorReason: "no available provider for model " + strings.Repeat("x", 4000)}

	msg := w.recordToMessage(&rec)
	if msg.ErrorReason == nil {
		t.Fatal("error_reason was dropped entirely")
	}
	if len(*msg.ErrorReason) > 512 {
		t.Fatalf("error_reason left the choke point at %d bytes", len(*msg.ErrorReason))
	}
	if !strings.HasPrefix(*msg.ErrorReason, "no available provider for model") {
		t.Fatalf("the bound must preserve the head: %q", (*msg.ErrorReason)[:40])
	}
}
