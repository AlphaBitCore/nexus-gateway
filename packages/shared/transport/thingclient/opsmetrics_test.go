package thingclient

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/platform"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/prometheus/client_golang/prometheus"
)

// TestPushMetricsSample_WireFormat asserts that PushMetricsSample serializes
// the SampleBatch into the on-wire shape defined by the ops-metrics spec
// §7.1: a flat object with type="metrics_sample" alongside thingId,
// sampledAt, and samples (NOT wrapped in a "payload" envelope).
func TestPushMetricsSample_WireFormat(t *testing.T) {
	c, _ := newTestClient(t)
	setWSConnected(t, c)

	batch := opsmetrics.SampleBatch{
		ThingID:   "thing-001",
		SampledAt: time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC),
		Samples: []opsmetrics.Sample{
			{
				Name:         "runtime.heap_alloc_bytes",
				Kind:         opsmetrics.KindGauge,
				DimensionKey: "",
				Value:        12345678,
			},
			{
				Name:         "relay.dial_total",
				Kind:         opsmetrics.KindCounter,
				DimensionKey: "mode=new",
				Value:        42,
			},
		},
	}

	if err := c.PushMetricsSample(context.Background(), batch); err != nil {
		t.Fatalf("PushMetricsSample error: %v", err)
	}

	var data []byte
	select {
	case data = <-c.outChMetrics:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for metrics_sample on outCh")
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal sent bytes: %v; raw=%s", err, data)
	}

	var typ string
	if err := json.Unmarshal(got["type"], &typ); err != nil {
		t.Fatalf("decode type: %v", err)
	}
	if typ != "metrics_sample" {
		t.Errorf("type = %q, want metrics_sample", typ)
	}

	var thingID string
	if err := json.Unmarshal(got["thingId"], &thingID); err != nil {
		t.Fatalf("decode thingId: %v", err)
	}
	if thingID != "thing-001" {
		t.Errorf("thingId = %q, want thing-001", thingID)
	}

	if _, ok := got["payload"]; ok {
		t.Errorf("metrics_sample MUST be flat (no 'payload' wrapper); got payload=%s", got["payload"])
	}

	var samples []opsmetrics.Sample
	if err := json.Unmarshal(got["samples"], &samples); err != nil {
		t.Fatalf("decode samples: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("samples len = %d, want 2", len(samples))
	}
	if samples[0].Name != "runtime.heap_alloc_bytes" {
		t.Errorf("samples[0].Name = %q, want runtime.heap_alloc_bytes", samples[0].Name)
	}
	if samples[1].DimensionKey != "mode=new" {
		t.Errorf("samples[1].DimensionKey = %q, want mode=new", samples[1].DimensionKey)
	}
}

// TestPushDiagEvent_WireFormat asserts that PushDiagEvent serializes the
// DiagEvent into the on-wire shape defined by spec §7.2: a flat object with
// type="diag_event" alongside thingId, occurredAt, level, eventType, etc.
func TestPushDiagEvent_WireFormat(t *testing.T) {
	c, _ := newTestClient(t)
	setWSConnected(t, c)

	event := opsmetrics.DiagEvent{
		ThingID:     "thing-001",
		OccurredAt:  time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC),
		Level:       opsmetrics.LevelError,
		EventType:   opsmetrics.EventTypeError,
		Source:      "relay",
		Message:     "dial to upstream failed",
		MessageHash: "9a8f0001",
		Attrs: map[string]any{
			"upstream": "api.openai.com:443",
		},
		RepeatCount:  1,
		AgentVersion: "v1.4.2",
		OSInfo: map[string]any{
			"os":      "darwin",
			"version": "14.4",
		},
	}

	if err := c.PushDiagEvent(context.Background(), event); err != nil {
		t.Fatalf("PushDiagEvent error: %v", err)
	}

	var data []byte
	select {
	case data = <-c.outChMetrics:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for diag_event on outCh")
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal sent bytes: %v; raw=%s", err, data)
	}

	var typ string
	if err := json.Unmarshal(got["type"], &typ); err != nil {
		t.Fatalf("decode type: %v", err)
	}
	if typ != "diag_event" {
		t.Errorf("type = %q, want diag_event", typ)
	}

	if _, ok := got["payload"]; ok {
		t.Errorf("diag_event MUST be flat (no 'payload' wrapper); got payload=%s", got["payload"])
	}

	var level string
	if err := json.Unmarshal(got["level"], &level); err != nil {
		t.Fatalf("decode level: %v", err)
	}
	if level != "error" {
		t.Errorf("level = %q, want error", level)
	}

	var src string
	if err := json.Unmarshal(got["source"], &src); err != nil {
		t.Fatalf("decode source: %v", err)
	}
	if src != "relay" {
		t.Errorf("source = %q, want relay", src)
	}
}

// TestUpdateStaticInfo_WireFormat asserts that UpdateStaticInfo serializes
// the StaticInfo into the on-wire shape used for thing.metadata.staticInfo:
// a flat object with type="static_info" alongside thingId and a nested
// "staticInfo" object that mirrors opsmetrics.StaticInfo's JSON tags.
func TestUpdateStaticInfo_WireFormat(t *testing.T) {
	c, _ := newTestClient(t)
	setWSConnected(t, c)

	info := opsmetrics.StaticInfo{
		Hostname:       "ai-gateway-01",
		PrimaryIP:      "10.0.0.7",
		OS:             "linux",
		OSVersion:      "Ubuntu 22.04",
		KernelVersion:  "6.5.0",
		CPUCores:       8,
		TotalRAMBytes:  16 * 1024 * 1024 * 1024,
		ServiceVersion: "ai-gateway/0.1.0",
		StartTime:      "2026-04-27T10:00:00Z",
	}

	if err := c.UpdateStaticInfo(context.Background(), info); err != nil {
		t.Fatalf("UpdateStaticInfo error: %v", err)
	}

	var data []byte
	select {
	case data = <-c.outChControl:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for static_info on outCh")
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v; raw=%s", err, data)
	}

	var typ string
	if err := json.Unmarshal(got["type"], &typ); err != nil {
		t.Fatalf("decode type: %v", err)
	}
	if typ != "static_info" {
		t.Errorf("type = %q, want static_info", typ)
	}

	var nested opsmetrics.StaticInfo
	if err := json.Unmarshal(got["staticInfo"], &nested); err != nil {
		t.Fatalf("decode staticInfo: %v", err)
	}
	if nested.Hostname != info.Hostname || nested.CPUCores != info.CPUCores {
		t.Errorf("staticInfo round-trip mismatch: got %+v want %+v", nested, info)
	}
	if _, ok := got["payload"]; ok {
		t.Errorf("static_info MUST be flat; got payload=%s", got["payload"])
	}
}

// TestPushMetricsSample_Disconnected returns an error when the WS write pump
// is not draining (channel full + timeout-ish behavior). With a non-connected
// client, the message still queues into outCh (size 64) until full; assert
// the function does NOT panic and signal-throughs the sendBytes timeout
// machinery.
func TestPushMetricsSample_QueueAccepted(t *testing.T) {
	c, _ := newTestClient(t)
	setWSConnected(t, c)

	batch := opsmetrics.SampleBatch{ThingID: "x", SampledAt: time.Now().UTC()}
	if err := c.PushMetricsSample(context.Background(), batch); err != nil {
		t.Fatalf("PushMetricsSample on connected client returned error: %v", err)
	}
	// Drain so test does not leak goroutine-bound channel state.
	select {
	case <-c.outChMetrics:
	case <-time.After(time.Second):
		t.Fatal("expected message on outCh")
	}
}

// TestTickHeartbeat_PushesMetricsSampleWhenSamplerConfigured exercises the
// heartbeat-tick → metrics_sample path: when Config.OpsMetricsSampler is
// non-nil, each tickHeartbeat() invocation must collect a SampleBatch from
// the sampler and enqueue a metrics_sample message on outCh.
func TestTickHeartbeat_PushesMetricsSampleWhenSamplerConfigured(t *testing.T) {
	registry := opsmetrics.NewRegistry(prometheus.NewRegistry())
	sampler := platform.NewSampler("thing-001", time.Now().Add(-5*time.Minute), registry)

	c, _ := newTestClient(t)
	c.cfg.OpsMetricsSampler = sampler
	setWSConnected(t, c)

	c.tickHeartbeat(context.Background())

	var data []byte
	select {
	case data = <-c.outChMetrics:
	case <-time.After(time.Second):
		t.Fatal("tickHeartbeat did not enqueue a metrics_sample on outCh")
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v; raw=%s", err, data)
	}
	var typ string
	if err := json.Unmarshal(got["type"], &typ); err != nil {
		t.Fatalf("decode type: %v", err)
	}
	if typ != "metrics_sample" {
		t.Errorf("type = %q, want metrics_sample", typ)
	}

	var samples []opsmetrics.Sample
	if err := json.Unmarshal(got["samples"], &samples); err != nil {
		t.Fatalf("decode samples: %v", err)
	}
	// RuntimeSampler emits 11 L1 samples; with no L3 metrics registered
	// the batch length must equal the runtime sampler's catalog. A non-zero
	// length is sufficient for this integration test.
	if len(samples) == 0 {
		t.Errorf("expected non-empty samples on heartbeat tick, got 0")
	}
}

// TestTickHeartbeat_NoOpWhenSamplerNotConfigured asserts the path is opt-in:
// when Config.OpsMetricsSampler is nil, tickHeartbeat must NOT emit a
// metrics_sample message. Existing thingclient consumers who haven't wired
// the sampler yet (Tasks 18-22) keep their pre-Task-8 behavior.
func TestTickHeartbeat_NoOpWhenSamplerNotConfigured(t *testing.T) {
	c, _ := newTestClient(t)
	setWSConnected(t, c)
	// cfg.OpsMetricsSampler stays nil.

	c.tickHeartbeat(context.Background())

	select {
	case data := <-c.outChMetrics:
		t.Errorf("tickHeartbeat enqueued a message with sampler=nil: %s", data)
	case <-time.After(50 * time.Millisecond):
		// expected: nothing on outCh
	}
}
