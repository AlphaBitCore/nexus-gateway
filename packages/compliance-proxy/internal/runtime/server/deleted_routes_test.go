package server

import (
	"bytes"
	"net/http"
	"testing"
)

// TestDeletedRoutes_Return404 is the compliance gate for Task 16: every
// legacy mutating surface (/killswitch*, /exemptions*, /alerts/*) must be
// fully removed from the runtime API. The test hits each known legacy path
// with the method it used to accept and asserts the server returns 404 (or
// any response other than the legacy 200/201/204) so nobody can regress the
// surface back into existence.
//
// Why 404 matters: net/http.ServeMux returns 404 for unregistered routes.
// If any path here dispatches to a registered handler (even one that replies
// with 405 or 503), the underlying route is still wired — the test fails.
func TestDeletedRoutes_Return404(t *testing.T) {
	deps := newTestDeps()
	ts := newTestServer(t, deps)
	defer ts.Close()

	cases := []struct {
		name   string
		method string
		path   string
		body   []byte
	}{
		{"get_killswitch", http.MethodGet, "/killswitch", nil},
		{"post_killswitch", http.MethodPost, "/killswitch", []byte(`{"engaged":false}`)},
		{"post_killswitch_force_close", http.MethodPost, "/killswitch/force-close", nil},
		{"get_killswitch_history", http.MethodGet, "/killswitch/history", nil},

		{"get_exemptions", http.MethodGet, "/exemptions", nil},
		{"post_exemptions", http.MethodPost, "/exemptions",
			[]byte(`{"sourceIp":"1.2.3.4","targetHost":"a","durationMinutes":5}`)},
		{"delete_exemption", http.MethodDelete, "/exemptions/abc", nil},

		{"get_alerts", http.MethodGet, "/alerts", nil},
		{"get_alerts_webhook", http.MethodGet, "/alerts/webhook", nil},
		{"put_alerts_webhook", http.MethodPut, "/alerts/webhook",
			[]byte(`{"url":"https://x","timeoutSec":10}`)},
		{"get_alerts_thresholds", http.MethodGet, "/alerts/thresholds", nil},
		{"put_alerts_thresholds", http.MethodPut, "/alerts/thresholds", []byte(`{}`)},
		{"get_alerts_channels", http.MethodGet, "/alerts/channels", nil},
		{"post_alerts_channels", http.MethodPost, "/alerts/channels", []byte(`{}`)},
		{"put_alerts_channel_by_id", http.MethodPut, "/alerts/channels/abc", []byte(`{}`)},
		{"delete_alerts_channel_by_id", http.MethodDelete, "/alerts/channels/abc", nil},
		{"get_alerts_custom_checks", http.MethodGet, "/alerts/custom-checks", nil},
		{"post_alerts_custom_checks", http.MethodPost, "/alerts/custom-checks", []byte(`{}`)},
		{"put_alerts_custom_check_by_id", http.MethodPut, "/alerts/custom-checks/abc", []byte(`{}`)},
		{"delete_alerts_custom_check_by_id", http.MethodDelete, "/alerts/custom-checks/abc", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body *bytes.Buffer
			if tc.body != nil {
				body = bytes.NewBuffer(tc.body)
			} else {
				body = &bytes.Buffer{}
			}
			req, err := http.NewRequest(tc.method, ts.URL+tc.path, body)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			if tc.body != nil {
				req.Header.Set("Content-Type", "application/json")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close() //nolint:errcheck

			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s: expected 404, got %d — legacy route still registered?",
					tc.method, tc.path, resp.StatusCode)
			}
		})
	}
}
