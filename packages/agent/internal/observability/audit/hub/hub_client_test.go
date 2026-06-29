// Coverage for hub_client.go: NewHubAuditClient construction +
// UploadAudit happy path / non-200 / decode failure / transport
// failure / ctx-cancel / header propagation (auth + X-Thing-Id).
package hub

import (
	"context"
	"github.com/goccy/go-json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewHubAuditClient_FieldsPopulated(t *testing.T) {
	c := NewHubAuditClient("https://hub.example.com", "tok", "thing-1")
	if c.BaseURL != "https://hub.example.com" {
		t.Errorf("BaseURL: got %q", c.BaseURL)
	}
	if c.DeviceToken != "tok" {
		t.Errorf("DeviceToken: got %q", c.DeviceToken)
	}
	if c.ThingID != "thing-1" {
		t.Errorf("ThingID: got %q", c.ThingID)
	}
	if c.HTTPClient == nil {
		t.Fatal("HTTPClient nil; httpclient.New should wire one")
	}
}

// TestUploadAudit_HappyPath drives a live httptest server end-to-end:
// verifies the POST URL, headers (Authorization Bearer, X-Thing-Id,
// Content-Type), request body shape (JSON array of maps), and
// response decoding into UploadAuditResponse.Accepted.
func TestUploadAudit_HappyPath(t *testing.T) {
	var (
		gotMethod, gotPath, gotAuth, gotThingID, gotCT string
		gotBody                                        []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotThingID = r.Header.Get("X-Thing-Id")
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(UploadAuditResponse{Accepted: []string{"e1", "e2"}})
	}))
	defer srv.Close()

	c := NewHubAuditClient(srv.URL, "deadbeef", "thing-abc")
	resp, err := c.UploadAudit(context.Background(), []map[string]any{
		{"id": "e1", "host": "api.openai.com"},
		{"id": "e2", "host": "api.anthropic.com"},
	})
	if err != nil {
		t.Fatalf("UploadAudit: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q, want POST", gotMethod)
	}
	if gotPath != "/api/internal/things/agent-audit" {
		t.Errorf("path: got %q", gotPath)
	}
	if gotAuth != "Bearer deadbeef" {
		t.Errorf("auth header: got %q", gotAuth)
	}
	if gotThingID != "thing-abc" {
		t.Errorf("X-Thing-Id: got %q", gotThingID)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type: got %q", gotCT)
	}
	if !strings.Contains(string(gotBody), "e1") || !strings.Contains(string(gotBody), "e2") {
		t.Errorf("body missing event IDs: %s", gotBody)
	}
	if len(resp.Accepted) != 2 || resp.Accepted[0] != "e1" {
		t.Errorf("accepted ids: got %v", resp.Accepted)
	}
}

// TestUploadAudit_Non200ReturnsErrorWithBody pins that the upstream
// error body is preserved (capped at 1 KiB by io.LimitReader) so the
// drain-loop retry can log meaningful Hub-side rejection reasons.
func TestUploadAudit_Non200ReturnsErrorWithBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"schema-violation"}`))
	}))
	defer srv.Close()
	c := NewHubAuditClient(srv.URL, "tok", "thing")
	_, err := c.UploadAudit(context.Background(), []map[string]any{{"x": 1}})
	if err == nil {
		t.Fatal("expected error on 400 response")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error must include status code; got %v", err)
	}
	if !strings.Contains(err.Error(), "schema-violation") {
		t.Errorf("error must include response body; got %v", err)
	}
}

// TestUploadAudit_DecodeFailureSurfacedAsError ensures a Hub returning
// 200 + non-JSON body produces a wrapped "decode audit response" error
// rather than silently returning empty Accepted (so drain-loop will
// retry the events instead of marking them synced).
func TestUploadAudit_DecodeFailureSurfacedAsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	c := NewHubAuditClient(srv.URL, "tok", "thing")
	_, err := c.UploadAudit(context.Background(), []map[string]any{{"x": 1}})
	if err == nil {
		t.Fatal("expected decode error on non-JSON response")
	}
	if !strings.Contains(err.Error(), "decode audit response") {
		t.Errorf("error should wrap decode failure; got %v", err)
	}
}

// TestUploadAudit_TransportErrorReturned simulates an unreachable Hub
// (server closed before request) — the HTTPClient.Do must surface a
// non-nil error.
func TestUploadAudit_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // close immediately to make subsequent calls fail.
	c := NewHubAuditClient(srv.URL, "tok", "thing")
	_, err := c.UploadAudit(context.Background(), []map[string]any{{"x": 1}})
	if err == nil {
		t.Fatal("expected transport error against closed server")
	}
	if !strings.Contains(err.Error(), "upload audit") {
		t.Errorf("error must wrap upload-audit context; got %v", err)
	}
}

// TestUploadAudit_ContextCancelled pins that a cancelled context aborts
// the request before the network round-trip lands.
func TestUploadAudit_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewHubAuditClient(srv.URL, "tok", "thing")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.UploadAudit(ctx, []map[string]any{{"x": 1}})
	if err == nil {
		t.Fatal("expected error on pre-cancelled context")
	}
}

// TestUploadAudit_InvalidBaseURLNewRequestError pins the
// http.NewRequestWithContext error branch by passing a BaseURL with an
// invalid control character that NewRequest's URL parser rejects.
func TestUploadAudit_InvalidBaseURLNewRequestError(t *testing.T) {
	c := NewHubAuditClient("http://example.com\x7f", "tok", "thing")
	_, err := c.UploadAudit(context.Background(), []map[string]any{{"x": 1}})
	if err == nil {
		t.Fatal("expected NewRequest error for invalid URL char")
	}
}

// TestUploadAudit_EmptyBatchStillPosts checks that an empty event slice
// is marshalled as "null" (Go's json.Marshal of nil slice) and the POST
// still issues — Hub-side decides whether to noop or 400; behaviour
// pinned here is "we always hit the network".
func TestUploadAudit_EmptyBatchStillPosts(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(UploadAuditResponse{Accepted: []string{}})
	}))
	defer srv.Close()
	c := NewHubAuditClient(srv.URL, "tok", "thing")
	resp, err := c.UploadAudit(context.Background(), nil)
	if err != nil {
		t.Fatalf("upload empty batch: %v", err)
	}
	if hits != 1 {
		t.Errorf("expected exactly one HTTP call, got %d", hits)
	}
	if len(resp.Accepted) != 0 {
		t.Errorf("Accepted should be empty: %v", resp.Accepted)
	}
}
