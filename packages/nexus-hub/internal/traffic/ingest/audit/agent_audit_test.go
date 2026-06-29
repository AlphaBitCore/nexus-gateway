package audit

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
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

func TestUploadAgentAudit_NoNormalizedUploadNoStamp(t *testing.T) {
	// The Hub no longer re-derives the normalized projection at ingest. When the
	// agent uploads only raw payloads (no governed normalized copies), the
	// envelope carries NO requestNormalized / responseNormalized / normalizeVersion —
	// the control plane recomputes the projection at view time from the stored
	// (already-redacted) body.
	mp := &mockProducer{}
	h := &AgentAuditAPI{MQProducer: mp}
	evs := []AgentAuditEvent{{
		ID:                         "e1",
		ProviderName:               "openai",
		ModelName:                  "gpt-4o",
		PayloadRequest:             []byte(`{"q":1}`),
		PayloadResponse:            []byte(`data: {}`),
		PayloadResponseContentType: "text/event-stream",
	}}
	rec := post(t, h, mustEvents(t, evs), true, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var env map[string]any
	_ = json.Unmarshal(mp.enqueued[0], &env)
	if _, ok := env["requestNormalized"]; ok {
		t.Errorf("requestNormalized must be absent when the agent uploaded none: %v", env["requestNormalized"])
	}
	if _, ok := env["responseNormalized"]; ok {
		t.Errorf("responseNormalized must be absent when the agent uploaded none: %v", env["responseNormalized"])
	}
	if _, ok := env["normalizeVersion"]; ok {
		t.Errorf("normalizeVersion must be absent when nothing stamped: %v", env["normalizeVersion"])
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

// TestUploadAgentAudit_RejectsForgedAttribution is the regression test: an
// enrolled agent that self-asserts a victim's entityId/orgId/identity/
// apiKeyFingerprint must NOT have those values propagated. The Hub stamps them
// empty (server-controlled) and keeps only the authenticated thing_id, so a
// rogue node cannot attribute its traffic to — or frame for SIEM — another
// VK/org. The forged keys are sent as raw agent wire (the decode struct no
// longer carries them; Go ignores the extra JSON keys).
func TestUploadAgentAudit_RejectsForgedAttribution(t *testing.T) {
	mp := &mockProducer{}
	h := &AgentAuditAPI{MQProducer: mp}
	body := `[{"id":"e1","entityType":"user","entityId":"victim-user","entityName":"Victim",` +
		`"orgId":"victim-org","orgName":"VictimCo","identity":{"sub":"victim"},` +
		`"apiKeyFingerprint":"victim-vk-fp","providerName":"openai","modelName":"gpt-4"}]`
	rec := post(t, h, body, true, "") // authenticated as thing-1
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if len(mp.enqueued) != 1 {
		t.Fatalf("expected 1 enqueued envelope, got %d", len(mp.enqueued))
	}
	var env map[string]any
	if err := json.Unmarshal(mp.enqueued[0], &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env["thingId"] != "thing-1" {
		t.Errorf("thingId = %v; want thing-1 (authenticated identity preserved)", env["thingId"])
	}
	for _, k := range []string{"entityType", "entityId", "entityName", "orgId", "orgName", "identity", "apiKeyFingerprint"} {
		if v, ok := env[k]; ok && v != "" && v != nil {
			t.Errorf("SEC-C5-01: forged %q leaked into the MQ envelope: %v", k, v)
		}
	}
}

// assertJSONEqual compares an envelope value against expected JSON
// semantically (the envelope round-trips through map[string]any, which
// reorders object keys).
func assertJSONEqual(t *testing.T, field string, got any, want string) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("%s: marshal got: %v", field, err)
	}
	var gotV, wantV any
	if err := json.Unmarshal(gotJSON, &gotV); err != nil {
		t.Fatalf("%s: unmarshal got: %v", field, err)
	}
	if err := json.Unmarshal([]byte(want), &wantV); err != nil {
		t.Fatalf("%s: unmarshal want: %v", field, err)
	}
	if !reflect.DeepEqual(gotV, wantV) {
		t.Errorf("%s = %s, want %s", field, gotJSON, want)
	}
}

// TestUploadAgentAudit_GovernedNormalizedPreferred — when the agent
// uploads its storage-governed normalized copies (span-redacted or
// drop-content placeholder) plus redaction spans, the Hub forwards them
// verbatim. The Hub never re-derives from raw bytes, so these uploaded
// copies are the only source of the normalized projection on this path.
func TestUploadAgentAudit_GovernedNormalizedPreferred(t *testing.T) {
	mp := &mockProducer{}
	h := &AgentAuditAPI{MQProducer: mp}
	governedReq := `{"kind":"ai-chat","messages":[{"role":"user","content":[{"type":"text","text":"[EMAIL-REDACTED]"}]}]}`
	governedResp := `{"kind":"ai-chat","redacted":true,"ruleIds":["pii-email"]}`
	reqSpans := `[{"start":0,"end":16,"replacement":"[EMAIL-REDACTED]","contentAddress":"messages.0.content.0"}]`
	evs := []AgentAuditEvent{{
		ID:                    "e1",
		ProviderName:          "openai",
		PayloadRequest:        []byte(`{"q":"leak@example.com"}`),
		PayloadResponse:       []byte(`{"a":1}`),
		NormalizedRequest:     json.RawMessage(governedReq),
		NormalizedResponse:    json.RawMessage(governedResp),
		RequestRedactionSpans: json.RawMessage(reqSpans),
	}}
	rec := post(t, h, mustEvents(t, evs), true, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var env map[string]any
	_ = json.Unmarshal(mp.enqueued[0], &env)
	assertJSONEqual(t, "requestNormalized", env["requestNormalized"], governedReq)
	assertJSONEqual(t, "responseNormalized", env["responseNormalized"], governedResp)
	assertJSONEqual(t, "requestRedactionSpans", env["requestRedactionSpans"], reqSpans)
	if _, ok := env["responseRedactionSpans"]; ok {
		t.Error("responseRedactionSpans must be absent when the agent stamped none")
	}
	if env["requestNormalizeStatus"] != "ok" || env["responseNormalizeStatus"] != "ok" {
		t.Errorf("normalize status = (%v,%v), want ok/ok", env["requestNormalizeStatus"], env["responseNormalizeStatus"])
	}
	if env["normalizeVersion"] != normcore.SchemaVersion {
		t.Errorf("normalizeVersion = %v, want %q", env["normalizeVersion"], normcore.SchemaVersion)
	}
}

// TestUploadAgentAudit_GovernedCopyOneDirectionOnly — a direction with an
// uploaded governed copy is forwarded; the other direction is left unstamped.
// The Hub no longer re-derives raw bytes, so the un-uploaded direction carries
// no normalized projection. normalizeVersion is still stamped because at least
// one direction was stamped.
func TestUploadAgentAudit_GovernedCopyOneDirectionOnly(t *testing.T) {
	mp := &mockProducer{}
	h := &AgentAuditAPI{MQProducer: mp}
	governedReq := `{"kind":"ai-chat","redacted":true}`
	evs := []AgentAuditEvent{{
		ID:                "e1",
		ProviderName:      "openai",
		PayloadRequest:    []byte(`{"q":1}`),
		PayloadResponse:   []byte(`{"a":1}`),
		NormalizedRequest: json.RawMessage(governedReq),
	}}
	post(t, h, mustEvents(t, evs), true, "")
	var env map[string]any
	_ = json.Unmarshal(mp.enqueued[0], &env)
	assertJSONEqual(t, "requestNormalized", env["requestNormalized"], governedReq)
	if _, ok := env["responseNormalized"]; ok {
		t.Errorf("responseNormalized must be absent — the Hub does not re-derive raw bytes: %v", env["responseNormalized"])
	}
	if env["normalizeVersion"] != normcore.SchemaVersion {
		t.Errorf("normalizeVersion = %v, want %q", env["normalizeVersion"], normcore.SchemaVersion)
	}
}
