package tlsbump

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
)

// TestReadBody_HonorsConfiguredLimit asserts that the payload-capture store
// drives the request body read cap end-to-end: a 10 KB request with a
// runtime cap of 4 KiB produces exactly 4096 bytes in memory, and changing
// the cap at runtime takes effect on the very next read.
func TestReadBody_HonorsConfiguredLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 10*1024)

	tests := []struct {
		name    string
		max     int64
		wantLen int
	}{
		{"4KiB cap truncates", 4096, 4096},
		{"zero falls back to default 10 MiB", 0, len(payload)},
		{"negative falls back to default 10 MiB", -1, len(payload)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(payload))
			got, err := readBody(req, tc.max)
			if err != nil {
				t.Fatalf("readBody: %v", err)
			}
			if len(got) != tc.wantLen {
				t.Errorf("len(body): want %d, got %d", tc.wantLen, len(got))
			}
		})
	}
}

// TestReadBody_UsesDynamicStoreValue verifies that a Store swap between
// reads changes the network read cap — the core guarantee the admin UI
// relies on when adjusting maxRequestBytes while traffic is live.
func TestReadBody_UsesDynamicStoreValue(t *testing.T) {
	store := payloadcapture.NewStore(payloadcapture.DefaultConfig())
	payload := bytes.Repeat([]byte("y"), 8192)

	// First request: use the default cap (10 MiB) so nothing truncates.
	req1 := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(payload))
	got1, err := readBody(req1, store.Get().MaxRequestBytes)
	if err != nil {
		t.Fatalf("first readBody: %v", err)
	}
	if len(got1) != len(payload) {
		t.Errorf("first read: want %d, got %d", len(payload), len(got1))
	}

	// Admin lowers the cap via a shadow invalidation.
	store.Set(payloadcapture.Config{MaxRequestBytes: 1024})

	req2 := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(payload))
	got2, err := readBody(req2, store.Get().MaxRequestBytes)
	if err != nil {
		t.Fatalf("second readBody: %v", err)
	}
	if len(got2) != 1024 {
		t.Errorf("second read after Set: want 1024, got %d", len(got2))
	}
}

// TestCaptureBodyIfEnabled is the single gate used by every Emit call
// site in the forward handler. A false flag or an empty body must yield
// nil so the audit event never carries a zero-length placeholder.
func TestCaptureBodyIfEnabled(t *testing.T) {
	body := []byte(`{"hello":"world"}`)

	tests := []struct {
		name    string
		enabled bool
		body    []byte
		wantNil bool
	}{
		{"disabled with body", false, body, true},
		{"enabled with body", true, body, false},
		{"enabled with empty body", true, []byte{}, true},
		{"enabled with nil body", true, nil, true},
		{"disabled with nil body", false, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := captureBodyIfEnabled(tc.enabled, tc.body)
			if tc.wantNil {
				if got != nil {
					t.Errorf("want nil, got %d bytes", len(got))
				}
			} else {
				if got == nil {
					t.Error("want non-nil, got nil")
				}
				if !bytes.Equal(got, tc.body) {
					t.Error("returned slice does not match input")
				}
			}
		})
	}
}
