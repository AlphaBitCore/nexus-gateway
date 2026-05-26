package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// dlqTestClient wires Client against the httptest server's URL with a
// known token; mirrors the helper pattern in client_test.go.
func dlqTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	return New(baseURL, "test-token", nil, nil)
}

func TestListDLQ_NotConfigured(t *testing.T) {
	c := dlqTestClient(t, "")
	_, _, err := c.ListDLQ(context.Background(), "", "", "")
	if err == nil {
		t.Fatal("expected ErrNotConfigured")
	}
}

func TestListDLQ_HappyPathForwardsQuery(t *testing.T) {
	var seenPath string
	var seenAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.RequestURI()
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rows":[]}`))
	}))
	defer ts.Close()

	c := dlqTestClient(t, ts.URL)
	body, status, err := c.ListDLQ(context.Background(), "nexus.event.compliance", "25", "2026-05-26T10:00:00Z")
	if err != nil {
		t.Fatalf("ListDLQ: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if !strings.Contains(string(body), `"rows":[]`) {
		t.Errorf("body = %s, want forwarded JSON", body)
	}
	if seenAuth != "Bearer test-token" {
		t.Errorf("auth = %q, want 'Bearer test-token'", seenAuth)
	}
	// All three filters must appear in the query string.
	for _, want := range []string{"subject=nexus.event.compliance", "limit=25", "cursor=2026-05-26T10"} {
		if !strings.Contains(seenPath, want) {
			t.Errorf("path %q missing %q", seenPath, want)
		}
	}
}

func TestListDLQ_EmptyFiltersOmittedFromQuery(t *testing.T) {
	var seenPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.RequestURI()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	c := dlqTestClient(t, ts.URL)
	if _, _, err := c.ListDLQ(context.Background(), "", "", ""); err != nil {
		t.Fatalf("ListDLQ: %v", err)
	}
	// All-empty inputs must produce a bare /api/hub/dlq path (no `?`).
	if strings.Contains(seenPath, "?") {
		t.Errorf("path = %q, want no query string for all-empty filters", seenPath)
	}
}

func TestListDLQ_MissingToken(t *testing.T) {
	c := New("http://localhost:1", "", nil, nil)
	_, _, err := c.ListDLQ(context.Background(), "", "", "")
	if err == nil {
		t.Fatal("expected error when token is empty")
	}
}

func TestRetryDLQ_NotConfigured(t *testing.T) {
	c := dlqTestClient(t, "")
	_, _, err := c.RetryDLQ(context.Background(), "abc")
	if err == nil {
		t.Fatal("expected ErrNotConfigured")
	}
}

func TestRetryDLQ_HappyPath(t *testing.T) {
	var seenMethod, seenPath, seenAuth, seenCT string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethod = r.Method
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		seenCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"subject":"nexus.event.gateway"}`))
	}))
	defer ts.Close()

	c := dlqTestClient(t, ts.URL)
	body, status, err := c.RetryDLQ(context.Background(), "row-123")
	if err != nil {
		t.Fatalf("RetryDLQ: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if !strings.Contains(string(body), `"ok":true`) {
		t.Errorf("body = %s, want forwarded ok:true", body)
	}
	if seenMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", seenMethod)
	}
	if !strings.HasSuffix(seenPath, "/api/hub/dlq/row-123/retry") {
		t.Errorf("path = %q, want suffix /api/hub/dlq/row-123/retry", seenPath)
	}
	if seenAuth != "Bearer test-token" {
		t.Errorf("auth header = %q", seenAuth)
	}
	if seenCT != "application/json" {
		t.Errorf("content-type = %q, want application/json", seenCT)
	}
}

func TestRetryDLQ_ForwardsNon2xx(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"dlq_not_found"}`))
	}))
	defer ts.Close()

	c := dlqTestClient(t, ts.URL)
	body, status, err := c.RetryDLQ(context.Background(), "missing")
	if err != nil {
		t.Fatalf("RetryDLQ: %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
	if !strings.Contains(string(body), "dlq_not_found") {
		t.Errorf("body = %s, want forwarded error envelope", body)
	}
}

func TestRetryDLQ_MissingToken(t *testing.T) {
	c := New("http://localhost:1", "", nil, nil)
	_, _, err := c.RetryDLQ(context.Background(), "abc")
	if err == nil {
		t.Fatal("expected error when token is empty")
	}
}
