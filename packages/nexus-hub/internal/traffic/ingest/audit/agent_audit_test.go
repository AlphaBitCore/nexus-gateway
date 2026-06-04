package audit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// mockProducer records Enqueue payloads and can fail after N successful calls.
type mockProducer struct {
	enqueued [][]byte
	enqErr   error
	enqAfter int // start returning enqErr once calls exceeds this count
	calls    int
}

func (m *mockProducer) Publish(context.Context, string, []byte) error { return nil }
func (m *mockProducer) Enqueue(_ context.Context, _ string, data []byte) error {
	m.calls++
	if m.enqErr != nil && m.calls > m.enqAfter {
		return m.enqErr
	}
	m.enqueued = append(m.enqueued, append([]byte(nil), data...))
	return nil
}
func (m *mockProducer) Close() error { return nil }

// post builds an echo context for a JSON request body and invokes UploadAgentAudit.
func post(t *testing.T, h *AgentAuditAPI, body string, setThing bool, hdrThingID string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/internal/things/agent-audit", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	if hdrThingID != "" {
		req.Header.Set("X-Thing-Id", hdrThingID)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if setThing {
		c.Set("thing", &store.Thing{ID: "thing-1", Name: "agent-1"})
	}
	if err := h.UploadAgentAudit(c); err != nil {
		t.Fatalf("UploadAgentAudit returned error: %v", err)
	}
	return rec
}

func mustEvents(t *testing.T, evs []AgentAuditEvent) string {
	t.Helper()
	b, err := json.Marshal(evs)
	if err != nil {
		t.Fatalf("marshal events: %v", err)
	}
	return string(b)
}

func TestUploadAgentAudit_QueueUnavailable(t *testing.T) {
	h := &AgentAuditAPI{MQProducer: nil}
	rec := post(t, h, `[{"id":"e1"}]`, true, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil producer should be 503, got %d", rec.Code)
	}
}

func TestUploadAgentAudit_BadBody(t *testing.T) {
	h := &AgentAuditAPI{MQProducer: &mockProducer{}}
	// Not a JSON array → bind error.
	rec := post(t, h, `{`, true, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body should be 400, got %d", rec.Code)
	}
	// Empty batch.
	rec = post(t, h, `[]`, true, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty batch should be 400, got %d", rec.Code)
	}
}

func TestUploadAgentAudit_TooLarge(t *testing.T) {
	h := &AgentAuditAPI{MQProducer: &mockProducer{}}
	evs := make([]AgentAuditEvent, maxAuditBatchSize+1)
	for i := range evs {
		evs[i] = AgentAuditEvent{ID: "e"}
	}
	rec := post(t, h, mustEvents(t, evs), true, "")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized batch should be 413, got %d", rec.Code)
	}
}

func TestUploadAgentAudit_HappyWithThingContext(t *testing.T) {
	mp := &mockProducer{}
	h := &AgentAuditAPI{MQProducer: mp}
	tt := 5
	evs := []AgentAuditEvent{{
		ID:               "e1",
		TraceID:          "tr1",
		ProviderName:     "openai",
		ModelName:        "gpt-4o",
		ErrorCode:        "UPSTREAM_5XX",
		ErrorReason:      "bad gateway",
		UpstreamTtfbMs:   &tt,
		UpstreamTotalMs:  &tt,
		RequestHooksMs:   &tt,
		ResponseHooksMs:  &tt,
		LatencyBreakdown: map[string]int{"upstream": 5},
		PayloadRequest:   []byte(`{"q":1}`),
		RequestSpillRef:  nil,
		ResponseSpillRef: &sharedaudit.SpillRef{Backend: "s3", Key: "k", Size: 1024, ContentType: "application/json"},
	}}
	rec := post(t, h, mustEvents(t, evs), true, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("happy should be 200, got %d", rec.Code)
	}
	var resp struct {
		Accepted []string `json:"accepted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if len(resp.Accepted) != 1 || resp.Accepted[0] != "e1" {
		t.Fatalf("accepted = %+v, want [e1]", resp.Accepted)
	}
	if len(mp.enqueued) != 1 {
		t.Fatalf("expected 1 enqueue, got %d", len(mp.enqueued))
	}
	// Envelope carries the thing identity + error fields + latency phases.
	var env map[string]any
	if err := json.Unmarshal(mp.enqueued[0], &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env["thingId"] != "thing-1" || env["thingName"] != "agent-1" {
		t.Fatalf("thing identity not stamped: %v / %v", env["thingId"], env["thingName"])
	}
	if env["errorCode"] != "UPSTREAM_5XX" || env["errorReason"] != "bad gateway" {
		t.Fatalf("error fields not stamped: %v", env)
	}
	if env["upstreamTtfbMs"] == nil || env["latencyBreakdown"] == nil {
		t.Fatalf("latency phases not stamped: %v", env)
	}
}

func TestUploadAgentAudit_HeaderThingFallbackAndEmptyID(t *testing.T) {
	mp := &mockProducer{}
	h := &AgentAuditAPI{MQProducer: mp}
	// No thing in context → header fallback; ID empty → not in accepted.
	evs := []AgentAuditEvent{{ID: ""}}
	rec := post(t, h, mustEvents(t, evs), false, "hdr-thing")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp struct {
		Accepted []string `json:"accepted"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Accepted) != 0 {
		t.Fatalf("empty-ID event must not be in accepted: %+v", resp.Accepted)
	}
	var env map[string]any
	if err := json.Unmarshal(mp.enqueued[0], &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env["thingId"] != "hdr-thing" || env["thingName"] != "" {
		t.Fatalf("header thing fallback failed: %v", env)
	}
}

func TestUploadAgentAudit_NormalizeStamping(t *testing.T) {
	var gotDirections []string
	mp := &mockProducer{}
	h := &AgentAuditAPI{
		MQProducer: mp,
		Normalize: func(direction, contentType, adapter, model, path string, stream bool, body []byte) (json.RawMessage, string, string) {
			gotDirections = append(gotDirections, direction)
			if direction == "response" && !stream {
				t.Fatalf("response with event-stream content type should set stream=true")
			}
			return json.RawMessage(`{"normalized":true}`), "ok", ""
		},
	}
	evs := []AgentAuditEvent{{
		ID:                         "e1",
		ProviderName:               "OpenAI", // upper → lowercased to adapter
		ModelName:                  "gpt-4o",
		PayloadRequest:             []byte(`{"q":1}`),
		PayloadResponse:            []byte(`data: {}`),
		PayloadResponseContentType: "text/event-stream",
	}}
	rec := post(t, h, mustEvents(t, evs), true, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if len(gotDirections) != 2 || gotDirections[0] != "request" || gotDirections[1] != "response" {
		t.Fatalf("normalize should run for request+response: %v", gotDirections)
	}
	var env map[string]any
	_ = json.Unmarshal(mp.enqueued[0], &env)
	if env["requestNormalized"] == nil || env["responseNormalized"] == nil || env["normalizeVersion"] != "1" {
		t.Fatalf("normalize fields not stamped: %v", env)
	}
}

func TestUploadAgentAudit_NormalizeResponseDefaultContentType(t *testing.T) {
	// Response payload with empty content type → defaults to application/json,
	// stream=false (no event-stream marker). Exercises the response-side
	// content-type default + non-stream branch.
	var sawResponse bool
	mp := &mockProducer{}
	h := &AgentAuditAPI{
		MQProducer: mp,
		Normalize: func(direction, contentType, _, _, _ string, stream bool, _ []byte) (json.RawMessage, string, string) {
			if direction == "response" {
				sawResponse = true
				if contentType != "application/json" || stream {
					t.Fatalf("response default ct/stream wrong: ct=%q stream=%v", contentType, stream)
				}
			}
			return json.RawMessage(`{}`), "ok", ""
		},
	}
	evs := []AgentAuditEvent{{
		ID:              "e1",
		ProviderName:    "gemini",
		PayloadResponse: []byte(`{"text":"hi"}`),
		// PayloadResponseContentType intentionally empty.
	}}
	post(t, h, mustEvents(t, evs), true, "")
	if !sawResponse {
		t.Fatal("normalize should have run for the response direction")
	}
}

func TestUploadAgentAudit_NormalizeSkippedAndNoStamp(t *testing.T) {
	// Normalize returns empty raw + empty status → not stamped (stamped stays false,
	// no normalizeVersion). Provider present so the block is entered.
	mp := &mockProducer{}
	h := &AgentAuditAPI{
		MQProducer: mp,
		Normalize: func(_, _, _, _, _ string, _ bool, _ []byte) (json.RawMessage, string, string) {
			return nil, "", ""
		},
	}
	evs := []AgentAuditEvent{{
		ID:             "e1",
		ProviderName:   "anthropic",
		PayloadRequest: []byte(`{"q":1}`),
	}}
	rec := post(t, h, mustEvents(t, evs), true, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var env map[string]any
	_ = json.Unmarshal(mp.enqueued[0], &env)
	if _, ok := env["normalizeVersion"]; ok {
		t.Fatalf("normalizeVersion must be absent when nothing stamped: %v", env)
	}
}

func TestUploadAgentAudit_NormalizeNilProviderEmpty(t *testing.T) {
	// Normalize non-nil but ProviderName empty → normalize block skipped entirely.
	called := false
	mp := &mockProducer{}
	h := &AgentAuditAPI{
		MQProducer: mp,
		Normalize: func(_, _, _, _, _ string, _ bool, _ []byte) (json.RawMessage, string, string) {
			called = true
			return nil, "", ""
		},
	}
	evs := []AgentAuditEvent{{ID: "e1", PayloadRequest: []byte(`{"q":1}`)}}
	post(t, h, mustEvents(t, evs), true, "")
	if called {
		t.Fatal("normalize must not run when providerName is empty")
	}
}

func TestUploadAgentAudit_EnqueueErrorBreaks(t *testing.T) {
	// First enqueue succeeds, second fails → loop breaks; only e1 accepted.
	mp := &mockProducer{enqErr: errors.New("mq down"), enqAfter: 1}
	h := &AgentAuditAPI{MQProducer: mp}
	evs := []AgentAuditEvent{{ID: "e1"}, {ID: "e2"}, {ID: "e3"}}
	rec := post(t, h, mustEvents(t, evs), true, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp struct {
		Accepted []string `json:"accepted"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Accepted) != 1 || resp.Accepted[0] != "e1" {
		t.Fatalf("only e1 should be accepted before break: %+v", resp.Accepted)
	}
}

func TestBuildAgentBody(t *testing.T) {
	// Spill ref → spill body.
	ref := &sharedaudit.SpillRef{Backend: "s3", Key: "k", Size: 99, ContentType: "application/json"}
	if b := buildAgentBody(nil, ref, "", false); b.Kind != sharedaudit.BodySpill {
		t.Fatalf("ref should produce spill body, got kind %q", b.Kind)
	}
	// Empty inline + nil ref → empty body.
	if b := buildAgentBody(nil, nil, "", false); b.Kind != sharedaudit.BodyAbsent {
		t.Fatalf("absent should produce empty body, got kind %q", b.Kind)
	}
	// Inline bytes → inline body.
	if b := buildAgentBody([]byte("hello"), nil, "text/plain", true); b.Kind != sharedaudit.BodyInline {
		t.Fatalf("inline bytes should produce inline body, got kind %q", b.Kind)
	}
}
