package expiry

import (
	"testing"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

// TestSeverityForCredExpiry covers all three urgency bands.
func TestSeverityForCredExpiry(t *testing.T) {
	cases := []struct {
		daysLeft int
		want     alerting.Severity
	}{
		{0, alerting.SeverityCritical},
		{1, alerting.SeverityCritical},
		{2, alerting.SeverityHigh},
		{7, alerting.SeverityHigh},
		{8, alerting.SeverityMedium},
		{14, alerting.SeverityMedium},
		{30, alerting.SeverityMedium},
	}
	for _, tc := range cases {
		got := severityForCredExpiry(tc.daysLeft)
		if got != tc.want {
			t.Errorf("severityForCredExpiry(%d) = %v; want %v", tc.daysLeft, got, tc.want)
		}
	}
}
