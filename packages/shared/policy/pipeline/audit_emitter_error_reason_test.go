package pipeline

import (
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/redact"
)

// classifyComplianceError is the only producer of traffic_event.error_reason,
// and it has four arms. Bounding one of them and calling the field bounded is
// how 5026 bytes reached a column documented as capped at 300: the two
// COMPLIANCE_BLOCKED arms returned CompliancePipelineResult.Reason verbatim,
// and that string comes from a third-party AI-Guard webhook's JSON response
// (webhook.go sets it straight from the wire), so its length is chosen
// outside this process entirely.
func TestClassifyComplianceError_EveryArmIsBounded(t *testing.T) {
	long := strings.Repeat("x", 5000)
	blocked := &CompliancePipelineResult{Decision: RejectHard, Reason: long}

	cases := []struct {
		name     string
		wantCode string
		call     func() (string, string)
	}{
		{"request pipeline blocked", "COMPLIANCE_BLOCKED", func() (string, string) {
			return classifyComplianceError(blocked, nil, "", 200, nil)
		}},
		{"response pipeline blocked", "COMPLIANCE_BLOCKED", func() (string, string) {
			return classifyComplianceError(nil, blocked, "", 200, nil)
		}},
		{"provider error", "PROVIDER_ERROR", func() (string, string) {
			return classifyComplianceError(nil, nil, "", 500,
				[]byte(`{"error":{"message":"`+long+`"}}`))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, reason := tc.call()
			if code != tc.wantCode {
				t.Fatalf("code = %q, want %q — the table is exercising the wrong arm", code, tc.wantCode)
			}
			if len(reason) > redact.MaxErrorReasonBytes+3 { // +3 for the ellipsis
				t.Errorf("%s produced %d bytes into traffic_event.error_reason; "+
					"the cap is %d and it belongs to the field, not to one arm",
					tc.name, len(reason), redact.MaxErrorReasonBytes)
			}
		})
	}
}
