package thingclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// #66 — UploadAgentAudit POSTs a raw JSON array to
// /api/internal/things/agent-audit (distinct from /things/audit which
// expects a cp envelope shape). Pre-fix agent was sending to the cp
// endpoint and Hub silently dropped PayloadRequest / PayloadResponse,
// so every body in cp-ui Traffic Detail was NULL. These tests pin
// the wire contract: correct path, correct body shape, success
// response decoded, retries on transient failure.

func TestUploadAgentAudit_Success(t *testing.T) {
	want := AuditBatchResponse{Ack: true, ConfirmedIDs: []string{"a", "b"}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/things/agent-audit" {
			t.Errorf("path = %q, want /api/internal/things/agent-audit", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
			t.Errorf("body should be raw JSON array, got: %q", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer ts.Close()

	c := newMQTestClientWithHTTP(t, ts.URL)
	resp, err := c.UploadAgentAudit(context.Background(), []byte(`[{"id":"a"},{"id":"b"}]`))
	if err != nil {
		t.Fatalf("UploadAgentAudit: %v", err)
	}
	if !resp.Ack || len(resp.ConfirmedIDs) != 2 {
		t.Errorf("response: %+v", resp)
	}
}

func TestUploadAgentAudit_HTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"server error"}`))
	}))
	defer ts.Close()

	c := newMQTestClientWithHTTP(t, ts.URL)
	if _, err := c.UploadAgentAudit(context.Background(), []byte(`[{"id":"x"}]`)); err == nil {
		t.Fatal("expected error on HTTP 500")
	}
}

func TestUploadAgentAuditWithRetry_RetriesOnFailure(t *testing.T) {
	var calls atomic.Int32
	want := AuditBatchResponse{Ack: true, ConfirmedIDs: []string{"id-1"}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer ts.Close()

	c := newMQTestClientWithHTTP(t, ts.URL)
	resp, err := c.UploadAgentAuditWithRetry(context.Background(), []byte(`[{"id":"id-1"}]`), 3)
	if err != nil {
		t.Fatalf("UploadAgentAuditWithRetry: %v", err)
	}
	if !resp.Ack || len(resp.ConfirmedIDs) != 1 {
		t.Errorf("response: %+v", resp)
	}
	if calls.Load() < 2 {
		t.Errorf("expected at least 2 calls (1 fail + 1 retry), got %d", calls.Load())
	}
}
