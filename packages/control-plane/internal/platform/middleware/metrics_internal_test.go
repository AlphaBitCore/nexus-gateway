package middleware

import "testing"

// TestStatusBucket covers every branch of the private status-class
// classifier, including the unknown bucket (status == 0 / negative /
// ≥ 600) that the integration test in metrics_test.go can't reach
// because Echo's response writer rejects out-of-range codes.
func TestStatusBucket(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{100, "1xx"},
		{199, "1xx"},
		{200, "2xx"},
		{299, "2xx"},
		{300, "3xx"},
		{399, "3xx"},
		{400, "4xx"},
		{499, "4xx"},
		{500, "5xx"},
		{599, "5xx"},
		{0, "unknown"},
		{600, "unknown"},
		{-1, "unknown"},
		{99, "unknown"},
		{999, "unknown"},
	}
	for _, tc := range cases {
		got := statusBucket(tc.status)
		if got != tc.want {
			t.Errorf("statusBucket(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}
