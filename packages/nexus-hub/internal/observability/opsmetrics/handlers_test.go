package opsmetrics

import (
	"context"
	"github.com/goccy/go-json"
	"sync"
	"testing"
	"time"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// fakeSampleWriter captures Enqueue calls for assertion.
type fakeSampleWriter struct {
	mu    sync.Mutex
	calls []sampleCall
}

type sampleCall struct {
	ThingID   string
	ThingType string
	Batch     opsmetrics.SampleBatch
}

func (w *fakeSampleWriter) Enqueue(_ context.Context, thingID, thingType string, batch opsmetrics.SampleBatch) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, sampleCall{ThingID: thingID, ThingType: thingType, Batch: batch})
	return nil
}

func (w *fakeSampleWriter) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.calls)
}

// fakeDiagWriter captures DiagEvent enqueue calls.
type fakeDiagWriter struct {
	mu    sync.Mutex
	calls []diagCall
}

type diagCall struct {
	ThingID   string
	ThingType string
	Event     opsmetrics.DiagEvent
}

func (w *fakeDiagWriter) Enqueue(_ context.Context, thingID, thingType string, evt opsmetrics.DiagEvent) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, diagCall{ThingID: thingID, ThingType: thingType, Event: evt})
	return nil
}

func (w *fakeDiagWriter) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.calls)
}

func TestMetricsSampleHandlerEnqueues(t *testing.T) {
	sw := &fakeSampleWriter{}
	dw := &fakeDiagWriter{}
	h := NewHandler(sw, dw, nil, nil)

	// flat-envelope shape per spec §7.1: type sits next to thingId/sampledAt/samples,
	// no "payload" wrapper.
	payload := map[string]any{
		"type":      "metrics_sample",
		"thingId":   "agent-abc",
		"sampledAt": "2026-04-27T10:00:00Z",
		"samples": []map[string]any{
			{"name": "runtime.heap_alloc_bytes", "kind": "gauge", "dim": "", "value": 12345.0},
			{"name": "relay.dial_total", "kind": "counter", "dim": "mode=new", "value": 42.0},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	if err := h.HandleMetricsSample(context.Background(), "agent-abc", "agent", raw); err != nil {
		t.Fatalf("HandleMetricsSample: %v", err)
	}

	if sw.callCount() != 1 {
		t.Fatalf("sample writer call count = %d, want 1", sw.callCount())
	}
	got := sw.calls[0]
	if got.ThingID != "agent-abc" {
		t.Errorf("ThingID = %q, want agent-abc", got.ThingID)
	}
	if got.ThingType != "agent" {
		t.Errorf("ThingType = %q, want agent", got.ThingType)
	}
	if len(got.Batch.Samples) != 2 {
		t.Fatalf("Samples len = %d, want 2", len(got.Batch.Samples))
	}
	if got.Batch.Samples[0].Name != "runtime.heap_alloc_bytes" {
		t.Errorf("sample[0].Name = %q", got.Batch.Samples[0].Name)
	}
	wantTime, _ := time.Parse(time.RFC3339, "2026-04-27T10:00:00Z")
	if !got.Batch.SampledAt.Equal(wantTime) {
		t.Errorf("SampledAt = %v, want %v", got.Batch.SampledAt, wantTime)
	}
	if dw.callCount() != 0 {
		t.Errorf("diag writer must not be called for metrics_sample")
	}
}

func TestMetricsSampleHandlerRejectsInvalidJSON(t *testing.T) {
	sw := &fakeSampleWriter{}
	dw := &fakeDiagWriter{}
	h := NewHandler(sw, dw, nil, nil)

	if err := h.HandleMetricsSample(context.Background(), "agent-abc", "agent", []byte("{not json")); err == nil {
		t.Fatal("expected error on invalid JSON, got nil")
	}
	if sw.callCount() != 0 {
		t.Errorf("sample writer must not be called on parse failure")
	}
}

func TestDiagEventHandlerEnqueues(t *testing.T) {
	sw := &fakeSampleWriter{}
	dw := &fakeDiagWriter{}
	h := NewHandler(sw, dw, nil, nil)

	payload := map[string]any{
		"type":         "diag_event",
		"thingId":      "agent-xyz",
		"occurredAt":   "2026-04-27T10:00:00Z",
		"level":        "error",
		"eventType":    "error",
		"source":       "relay",
		"message":      "dial to upstream failed",
		"messageHash":  "9a8f00",
		"attrs":        map[string]any{"upstream": "api.openai.com:443"},
		"repeatCount":  1,
		"agentVersion": "v1.4.2",
		"osInfo":       map[string]any{"os": "darwin"},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	if err := h.HandleDiagEvent(context.Background(), "agent-xyz", "agent", raw); err != nil {
		t.Fatalf("HandleDiagEvent: %v", err)
	}

	if dw.callCount() != 1 {
		t.Fatalf("diag writer call count = %d, want 1", dw.callCount())
	}
	got := dw.calls[0]
	if got.ThingID != "agent-xyz" {
		t.Errorf("ThingID = %q", got.ThingID)
	}
	if got.ThingType != "agent" {
		t.Errorf("ThingType = %q", got.ThingType)
	}
	if got.Event.Level != "error" || got.Event.Source != "relay" || got.Event.Message != "dial to upstream failed" {
		t.Errorf("unexpected event payload: %+v", got.Event)
	}
	if got.Event.MessageHash != "9a8f00" {
		t.Errorf("MessageHash = %q", got.Event.MessageHash)
	}
	if got.Event.RepeatCount != 1 {
		t.Errorf("RepeatCount = %d, want 1", got.Event.RepeatCount)
	}
	if sw.callCount() != 0 {
		t.Errorf("sample writer must not be called for diag_event")
	}
}

func TestDiagEventHandlerRejectsInvalidJSON(t *testing.T) {
	sw := &fakeSampleWriter{}
	dw := &fakeDiagWriter{}
	h := NewHandler(sw, dw, nil, nil)

	if err := h.HandleDiagEvent(context.Background(), "agent-xyz", "agent", []byte("xxx")); err == nil {
		t.Fatal("expected error on invalid JSON, got nil")
	}
	if dw.callCount() != 0 {
		t.Errorf("diag writer must not be called on parse failure")
	}
}

// fakeStaticInfoStore captures UpsertStaticInfo calls.
type fakeStaticInfoStore struct {
	mu       sync.Mutex
	calls    []staticInfoCall
	returnEr error
}

type staticInfoCall struct {
	ThingID string
	Info    opsmetrics.StaticInfo
}

func (s *fakeStaticInfoStore) UpsertStaticInfo(_ context.Context, thingID string, info opsmetrics.StaticInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, staticInfoCall{ThingID: thingID, Info: info})
	return s.returnEr
}

func TestStaticInfoHandlerUpserts(t *testing.T) {
	sw := &fakeSampleWriter{}
	dw := &fakeDiagWriter{}
	ss := &fakeStaticInfoStore{}
	h := NewHandler(sw, dw, ss, nil)

	payload := map[string]any{
		"type":    "static_info",
		"thingId": "ai-gateway-01",
		"staticInfo": map[string]any{
			"hostname":       "ai-gateway-01",
			"primaryIp":      "10.0.0.7",
			"os":             "linux",
			"cpuCores":       8,
			"totalRamBytes":  uint64(16 * 1024 * 1024 * 1024),
			"serviceVersion": "ai-gateway/0.1.0",
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := h.HandleStaticInfo(context.Background(), "ai-gateway-01", "ai-gateway", raw); err != nil {
		t.Fatalf("HandleStaticInfo: %v", err)
	}

	if len(ss.calls) != 1 {
		t.Fatalf("expected 1 UpsertStaticInfo call, got %d", len(ss.calls))
	}
	got := ss.calls[0]
	if got.ThingID != "ai-gateway-01" {
		t.Errorf("ThingID = %q, want ai-gateway-01", got.ThingID)
	}
	if got.Info.Hostname != "ai-gateway-01" || got.Info.CPUCores != 8 {
		t.Errorf("Info round-trip mismatch: got %+v", got.Info)
	}
}

func TestStaticInfoHandlerNoopWhenStoreNil(t *testing.T) {
	sw := &fakeSampleWriter{}
	dw := &fakeDiagWriter{}
	h := NewHandler(sw, dw, nil, nil)

	raw := []byte(`{"type":"static_info","thingId":"x","staticInfo":{"hostname":"x"}}`)
	// Must not return an error or panic; nil store is "feature unavailable" not "broken".
	if err := h.HandleStaticInfo(context.Background(), "x", "ai-gateway", raw); err != nil {
		t.Fatalf("HandleStaticInfo with nil store: %v", err)
	}

	// The static_info time stamp on the thing.metadata.staticInfo column also
	// records when Hub last received the payload; tests of that column live
	// in the writer-level integration test (StaticInfoWriter), not here.
	_ = time.Now()
}
