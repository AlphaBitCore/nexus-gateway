package rollup

import "testing"

// A rate over a handful of requests is not a rate.
//
// With no floor, ONE failed request in the thirty-minute window put a provider
// at 100% error and marked it unavailable — which fired the provider.unavailable
// alert and, in the UI, condemned a provider nothing was wrong with. The
// operator then had nothing to click: this status is DERIVED from the window
// and cannot be reset by hand, so it looked stuck until the window rolled the
// request off.
func TestHealthStatus_NeedsEvidenceBeforeCondemningAProvider(t *testing.T) {
	rate := func(errors, total int) float64 { return float64(errors) / float64(total) }

	for _, tc := range []struct {
		name          string
		total, errors int
		want          string
	}{
		{"one request, and it failed", 1, 1, "healthy"},
		{"three requests, all failed", 3, 3, "healthy"},
		{"just under the floor, all failed", 19, 19, "healthy"},
		{"at the floor, past the unavailable threshold", 20, 10, "unavailable"},
		{"at the floor, past the degraded threshold only", 20, 2, "degraded"},
		{"at the floor and clean", 20, 0, "healthy"},
		{"ample traffic, one failure", 500, 1, "healthy"},
		{"ample traffic, genuinely broken", 500, 400, "unavailable"},
		{"ample traffic, mildly degraded", 500, 50, "degraded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := healthStatus(tc.total, rate(tc.errors, tc.total)); got != tc.want {
				t.Errorf("%d/%d errors → %q, want %q", tc.errors, tc.total, got, tc.want)
			}
		})
	}
}

// The floor bites on the adverse verdicts only. A provider with no traffic at
// all must not read as broken either.
func TestHealthStatus_NoTrafficIsNotAVerdict(t *testing.T) {
	if got := healthStatus(0, 0); got != "healthy" {
		t.Errorf("an untouched provider reads %q", got)
	}
}
