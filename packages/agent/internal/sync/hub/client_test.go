package hub

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c, err := NewClient(Config{HubURL: baseURL, Timeout: 5 * time.Second, MaxRetries: 0})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestNewClient_RequiresHubURL(t *testing.T) {
	if _, err := NewClient(Config{}); err == nil {
		t.Fatal("expected error when HubURL is empty")
	}
}

func TestUploadAudit_SendsDeviceIdAndEvents(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/things/audit" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"ack": true, "accepted": 2})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	accepted, err := c.UploadAudit(context.Background(), "dev-1", []AuditEvent{
		{ID: "e1", Timestamp: time.Now(), Action: "inspect"},
		{ID: "e2", Timestamp: time.Now(), Action: "passthrough"},
	})
	if err != nil {
		t.Fatalf("UploadAudit: %v", err)
	}
	if accepted != 2 {
		t.Errorf("expected accepted=2, got %d", accepted)
	}
	if gotBody["thingId"] != "dev-1" {
		t.Errorf("expected thingId=dev-1, got %v", gotBody["thingId"])
	}
	events, ok := gotBody["events"].([]any)
	if !ok || len(events) != 2 {
		t.Errorf("expected 2 events, got %v", gotBody["events"])
	}
}

func TestUploadAudit_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.UploadAudit(context.Background(), "dev-1", []AuditEvent{{ID: "e1"}})
	if err == nil {
		t.Fatal("expected error on 400")
	}
}

func TestUploadExemption_SendsThingId(t *testing.T) {
	var gotBody ExemptionUpload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/things/exemption" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]bool{"ack": true})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	err := c.UploadExemption(context.Background(), ExemptionUpload{
		ThingID:   "dev-1",
		Host:      "api.openai.com",
		Reason:    "tls-bump-failed",
		ExpiresAt: time.Now().Add(1 * time.Hour),
	})
	if err != nil {
		t.Fatalf("UploadExemption: %v", err)
	}
	if gotBody.ThingID != "dev-1" {
		t.Errorf("expected thingId=dev-1, got %q", gotBody.ThingID)
	}
	if gotBody.Host != "api.openai.com" {
		t.Errorf("expected host=api.openai.com, got %q", gotBody.Host)
	}
}

func TestCheckUpdate_ReturnsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/things/update-check" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("currentVersion") != "1.0.0" {
			t.Errorf("missing currentVersion query")
		}
		// The osName parameter is retained in the method signature for
		// future per-OS pinning but must not appear on the wire today.
		if got := r.URL.Query().Get("os"); got != "" {
			t.Errorf("expected no 'os' query param on the wire, got %q", got)
		}
		_ = json.NewEncoder(w).Encode(UpdateInfo{Available: false})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	info, err := c.CheckUpdate(context.Background(), "1.0.0", "darwin")
	if err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
	if info.Available {
		t.Error("expected unavailable")
	}
}

func TestCheckUpdate_ReturnsAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(UpdateInfo{
			Available:   true,
			Version:     "2.0.0",
			DownloadURL: "https://example.com/agent",
			SHA256:      "deadbeef",
			Signature:   "sig==",
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	info, err := c.CheckUpdate(context.Background(), "1.0.0", "linux")
	if err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
	if !info.Available || info.Version != "2.0.0" || info.SHA256 != "deadbeef" {
		t.Errorf("unexpected info: %+v", info)
	}
}

func TestRenewDeviceToken_AuthenticatesAndReturnsRotatedToken(t *testing.T) {
	var gotAuth, gotThingID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/things/renew-token" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotThingID = r.Header.Get("X-Thing-Id")
		_ = json.NewEncoder(w).Encode(RenewTokenResponse{
			DeviceToken:          "new-token",
			DeviceTokenExpiresAt: "2026-07-07T00:00:00Z",
		})
	}))
	defer srv.Close()

	// The current token + thing id must ride on the rotation call (the call
	// authenticates with the still-valid token — refresh-while-valid).
	c, err := NewClient(Config{
		HubURL:        srv.URL,
		Timeout:       5 * time.Second,
		MaxRetries:    0,
		DeviceTokenFn: func() string { return "current-token" },
		ThingIDFn:     func() string { return "agent-9" },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	resp, err := c.RenewDeviceToken(context.Background())
	if err != nil {
		t.Fatalf("RenewDeviceToken: %v", err)
	}
	if resp.DeviceToken != "new-token" || resp.DeviceTokenExpiresAt != "2026-07-07T00:00:00Z" {
		t.Errorf("unexpected response: %+v", resp)
	}
	if gotAuth != "Bearer current-token" {
		t.Errorf("expected Bearer current-token, got %q", gotAuth)
	}
	if gotThingID != "agent-9" {
		t.Errorf("expected X-Thing-Id=agent-9, got %q", gotThingID)
	}
}

func TestRenewDeviceToken_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid or revoked device token"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.RenewDeviceToken(context.Background())
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention status 401: %v", err)
	}
}

func TestRenewDeviceToken_MalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.RenewDeviceToken(context.Background())
	if err == nil || !strings.Contains(err.Error(), "decode renew device token") {
		t.Fatalf("expected decode error, got %v", err)
	}
}

func TestCheckUpdate_EscapesQueryParam(t *testing.T) {
	// Version strings that contain reserved characters (e.g. '+', ' ') must
	// survive the round trip through url.Values.Encode.
	const messyVersion = "1.0.0+rc1 build/42"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("currentVersion"); got != messyVersion {
			t.Errorf("currentVersion round-trip: got %q want %q", got, messyVersion)
		}
		_ = json.NewEncoder(w).Encode(UpdateInfo{Available: false})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if _, err := c.CheckUpdate(context.Background(), messyVersion, ""); err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
}

func TestDoWithRetry_ContextCancelledMidRequest(t *testing.T) {
	// Server sleeps long enough for the client to cancel mid-request. The
	// returned error must unwrap to context.Canceled so callers can detect
	// shutdowns; it must not be wrapped in "request failed after N retries".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(500 * time.Millisecond):
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	c, err := NewClient(Config{
		HubURL:     srv.URL,
		Timeout:    5 * time.Second,
		MaxRetries: 3,
		RetryDelay: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)

	_, err = c.CheckUpdate(ctx, "1.0.0", "darwin")
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected errors.Is(err, context.Canceled); got %T: %v", err, err)
	}
}

func TestDoWithRetry_RetriesOn5xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(UpdateInfo{Available: false})
	}))
	defer srv.Close()

	c, err := NewClient(Config{
		HubURL:     srv.URL,
		Timeout:    5 * time.Second,
		MaxRetries: 2,
		RetryDelay: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.CheckUpdate(context.Background(), "1.0.0", "darwin"); err != nil {
		t.Fatalf("expected retry to succeed: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("expected 2 calls, got %d", calls.Load())
	}
}
