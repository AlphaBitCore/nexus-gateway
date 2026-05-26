package thingclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// --- Mock Producer ---

type mockProducer struct {
	mu       sync.Mutex
	enqueued []struct {
		Queue string
		Data  []byte
	}
	failNext    int
	enqueueFunc func(ctx context.Context, queue string, data []byte) error
}

func (m *mockProducer) Publish(_ context.Context, _ string, _ []byte) error { return nil }

func (m *mockProducer) Enqueue(ctx context.Context, queue string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.enqueueFunc != nil {
		return m.enqueueFunc(ctx, queue, data)
	}
	if m.failNext > 0 {
		m.failNext--
		return fmt.Errorf("mq unavailable")
	}
	m.enqueued = append(m.enqueued, struct {
		Queue string
		Data  []byte
	}{queue, data})
	return nil
}

func (m *mockProducer) Close() error { return nil }

func (m *mockProducer) getEnqueued() []struct {
	Queue string
	Data  []byte
} {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]struct {
		Queue string
		Data  []byte
	}, len(m.enqueued))
	copy(out, m.enqueued)
	return out
}

// --- Test Helpers ---

func newMQTestClient(t *testing.T, mp *mockProducer) *Client {
	t.Helper()
	reg := prometheus.NewRegistry()
	cfg := Config{
		HubURL:            "ws://localhost:9999/ws",
		ThingType:         "test-service",
		ThingID:           "test-001",
		Token:             "test-token",
		Logger:            slog.Default(),
		MetricsRegisterer: reg,
		MetricsNamespace:  "test",
		MQProducer:        mp,
		MQBufferSize:      100,
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func newMQTestClientWithHTTP(t *testing.T, serverURL string) *Client {
	t.Helper()
	reg := prometheus.NewRegistry()
	cfg := Config{
		HubURL:            "ws://localhost:9999/ws",
		HubHTTPURL:        serverURL,
		ThingType:         "agent",
		ThingID:           "agent-001",
		Token:             "test-token",
		Logger:            slog.Default(),
		MetricsRegisterer: reg,
		MetricsNamespace:  "test",
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// --- PublishEvent ---

func TestPublishEvent_Success(t *testing.T) {
	mp := &mockProducer{}
	c := newMQTestClient(t, mp)

	err := c.PublishEvent(context.Background(), "nexus.audit", []byte(`{"event":"test"}`))
	if err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}

	enqueued := mp.getEnqueued()
	if len(enqueued) != 1 {
		t.Fatalf("expected 1 enqueued event, got %d", len(enqueued))
	}
	if enqueued[0].Queue != "nexus.audit" {
		t.Errorf("queue = %q, want %q", enqueued[0].Queue, "nexus.audit")
	}
	if string(enqueued[0].Data) != `{"event":"test"}` {
		t.Errorf("data = %q, want %q", enqueued[0].Data, `{"event":"test"}`)
	}

	val := testutil.ToFloat64(c.promMetrics.mqPublished.WithLabelValues("nexus.audit"))
	if val != 1 {
		t.Errorf("mqPublished counter = %f, want 1", val)
	}
}

func TestPublishEvent_NoProducer(t *testing.T) {
	reg := prometheus.NewRegistry()
	cfg := Config{
		HubURL:            "ws://localhost:9999/ws",
		ThingType:         "agent",
		ThingID:           "agent-001",
		Token:             "test-token",
		Logger:            slog.Default(),
		MetricsRegisterer: reg,
		MetricsNamespace:  "test",
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = c.PublishEvent(context.Background(), "nexus.audit", []byte(`{}`))
	if err == nil {
		t.Fatal("expected error when MQProducer is nil")
	}
}

func TestPublishEvent_MQFailure_Buffers(t *testing.T) {
	mp := &mockProducer{failNext: 1}
	c := newMQTestClient(t, mp)
	c.mqBuffer = newRingBuffer(100, c.promMetrics, c.logger)

	err := c.PublishEvent(context.Background(), "nexus.audit", []byte(`{"id":"1"}`))
	if err == nil {
		t.Fatal("expected error on MQ failure")
	}

	if c.mqBuffer.Len() != 1 {
		t.Fatalf("expected 1 buffered event, got %d", c.mqBuffer.Len())
	}

	evt, ok := c.mqBuffer.Pop()
	if !ok {
		t.Fatal("expected to pop event from buffer")
	}
	if evt.Queue != "nexus.audit" {
		t.Errorf("buffered queue = %q, want %q", evt.Queue, "nexus.audit")
	}
	if string(evt.Data) != `{"id":"1"}` {
		t.Errorf("buffered data = %q, want %q", evt.Data, `{"id":"1"}`)
	}
}

// --- Ring Buffer ---

func TestRingBuffer_PushPop(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := newClientMetrics(reg, "test")
	rb := newRingBuffer(10, metrics, slog.Default())

	rb.Push(bufferedEvent{Queue: "q1", Data: []byte("d1")})
	rb.Push(bufferedEvent{Queue: "q2", Data: []byte("d2")})
	rb.Push(bufferedEvent{Queue: "q3", Data: []byte("d3")})

	want := []string{"q1", "q2", "q3"}
	for i, wq := range want {
		evt, ok := rb.Pop()
		if !ok {
			t.Fatalf("Pop %d: expected event", i)
		}
		if evt.Queue != wq {
			t.Errorf("Pop %d: queue = %q, want %q", i, evt.Queue, wq)
		}
	}

	_, ok := rb.Pop()
	if ok {
		t.Error("expected Pop to return false on empty buffer")
	}
}

func TestRingBuffer_Overflow(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := newClientMetrics(reg, "test")
	rb := newRingBuffer(3, metrics, slog.Default())

	rb.Push(bufferedEvent{Queue: "q1", Data: []byte("d1")})
	rb.Push(bufferedEvent{Queue: "q2", Data: []byte("d2")})
	rb.Push(bufferedEvent{Queue: "q3", Data: []byte("d3")})
	rb.Push(bufferedEvent{Queue: "q4", Data: []byte("d4")})

	if rb.Len() != 3 {
		t.Errorf("Len = %d, want 3", rb.Len())
	}

	dropped := testutil.ToFloat64(metrics.mqDropped)
	if dropped != 1 {
		t.Errorf("mqDropped = %f, want 1", dropped)
	}

	evt, ok := rb.Pop()
	if !ok {
		t.Fatal("expected event from Pop")
	}
	if evt.Queue != "q2" {
		t.Errorf("oldest remaining queue = %q, want %q (q1 should have been dropped)", evt.Queue, "q2")
	}
}

func TestRingBuffer_DrainAll(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := newClientMetrics(reg, "test")
	rb := newRingBuffer(10, metrics, slog.Default())

	for i := range 5 {
		rb.Push(bufferedEvent{Queue: fmt.Sprintf("q%d", i), Data: []byte(fmt.Sprintf("d%d", i))})
	}

	events := rb.DrainAll()
	if len(events) != 5 {
		t.Fatalf("DrainAll returned %d events, want 5", len(events))
	}

	for i, evt := range events {
		wantQ := fmt.Sprintf("q%d", i)
		if evt.Queue != wantQ {
			t.Errorf("event[%d].Queue = %q, want %q", i, evt.Queue, wantQ)
		}
	}

	if rb.Len() != 0 {
		t.Errorf("Len after DrainAll = %d, want 0", rb.Len())
	}

	gauge := testutil.ToFloat64(metrics.mqBufferSize)
	if gauge != 0 {
		t.Errorf("mqBufferSize gauge = %f, want 0", gauge)
	}
}

func TestRingBuffer_Concurrent(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := newClientMetrics(reg, "test")
	rb := newRingBuffer(1000, metrics, slog.Default())

	var wg sync.WaitGroup

	for i := range 10 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := range 100 {
				rb.Push(bufferedEvent{
					Queue: fmt.Sprintf("q%d", id),
					Data:  []byte(fmt.Sprintf("d%d-%d", id, j)),
				})
			}
		}(i)
	}

	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				rb.Pop()
			}
		}()
	}

	wg.Wait()

	remaining := rb.Len()
	if remaining < 0 || remaining > 1000 {
		t.Errorf("unexpected buffer length: %d", remaining)
	}
}

func TestRingBuffer_GaugeUpdates(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := newClientMetrics(reg, "test")
	rb := newRingBuffer(10, metrics, slog.Default())

	rb.Push(bufferedEvent{Queue: "q1", Data: []byte("d1")})
	if g := testutil.ToFloat64(metrics.mqBufferSize); g != 1 {
		t.Errorf("after Push: gauge = %f, want 1", g)
	}

	rb.Push(bufferedEvent{Queue: "q2", Data: []byte("d2")})
	if g := testutil.ToFloat64(metrics.mqBufferSize); g != 2 {
		t.Errorf("after second Push: gauge = %f, want 2", g)
	}

	rb.Pop()
	if g := testutil.ToFloat64(metrics.mqBufferSize); g != 1 {
		t.Errorf("after Pop: gauge = %f, want 1", g)
	}

	rb.Pop()
	if g := testutil.ToFloat64(metrics.mqBufferSize); g != 0 {
		t.Errorf("after final Pop: gauge = %f, want 0", g)
	}
}

// --- Buffer Drain ---

func TestBufferDrainLoop_Success(t *testing.T) {
	mp := &mockProducer{}
	c := newMQTestClient(t, mp)
	c.mqBuffer = newRingBuffer(100, c.promMetrics, c.logger)

	c.mqBuffer.Push(bufferedEvent{Queue: "q1", Data: []byte("d1")})
	c.mqBuffer.Push(bufferedEvent{Queue: "q2", Data: []byte("d2")})
	c.mqBuffer.Push(bufferedEvent{Queue: "q3", Data: []byte("d3")})

	c.drainBuffer(context.Background())

	if c.mqBuffer.Len() != 0 {
		t.Errorf("buffer Len after drain = %d, want 0", c.mqBuffer.Len())
	}

	enqueued := mp.getEnqueued()
	if len(enqueued) != 3 {
		t.Fatalf("expected 3 enqueued events, got %d", len(enqueued))
	}
	if enqueued[0].Queue != "q1" || enqueued[1].Queue != "q2" || enqueued[2].Queue != "q3" {
		t.Errorf("enqueued order: [%s, %s, %s], want [q1, q2, q3]",
			enqueued[0].Queue, enqueued[1].Queue, enqueued[2].Queue)
	}

	published := testutil.ToFloat64(c.promMetrics.mqPublished.WithLabelValues("q1"))
	if published != 1 {
		t.Errorf("mqPublished for q1 = %f, want 1", published)
	}
}

func TestBufferDrainLoop_StopsOnFailure(t *testing.T) {
	calls := 0
	mp := &mockProducer{
		enqueueFunc: func(_ context.Context, _ string, _ []byte) error {
			calls++
			if calls >= 2 {
				return fmt.Errorf("mq unavailable")
			}
			return nil
		},
	}
	c := newMQTestClient(t, mp)
	c.mqBuffer = newRingBuffer(100, c.promMetrics, c.logger)

	c.mqBuffer.Push(bufferedEvent{Queue: "q1", Data: []byte("d1")})
	c.mqBuffer.Push(bufferedEvent{Queue: "q2", Data: []byte("d2")})
	c.mqBuffer.Push(bufferedEvent{Queue: "q3", Data: []byte("d3")})

	c.drainBuffer(context.Background())

	if calls != 2 {
		t.Errorf("Enqueue calls = %d, want 2 (1 success + 1 failure)", calls)
	}
	if c.mqBuffer.Len() != 2 {
		t.Errorf("remaining buffer Len = %d, want 2", c.mqBuffer.Len())
	}
}

func TestFlushMQBuffer_Shutdown(t *testing.T) {
	mp := &mockProducer{}
	c := newMQTestClient(t, mp)
	c.mqBuffer = newRingBuffer(100, c.promMetrics, c.logger)

	c.mqBuffer.Push(bufferedEvent{Queue: "q1", Data: []byte("d1")})
	c.mqBuffer.Push(bufferedEvent{Queue: "q2", Data: []byte("d2")})
	c.mqBuffer.Push(bufferedEvent{Queue: "q3", Data: []byte("d3")})

	c.flushMQBuffer(context.Background())

	if c.mqBuffer.Len() != 0 {
		t.Errorf("buffer Len after flush = %d, want 0", c.mqBuffer.Len())
	}

	enqueued := mp.getEnqueued()
	if len(enqueued) != 3 {
		t.Fatalf("expected 3 flushed events, got %d", len(enqueued))
	}
}

// --- Audit Upload ---

func TestUploadAudit_Success(t *testing.T) {
	wantResp := AuditBatchResponse{
		Ack:          true,
		ConfirmedIDs: []string{"id-1", "id-2"},
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/things/audit" {
			t.Errorf("path = %q, want /api/internal/things/audit", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("Authorization = %q, want %q", r.Header.Get("Authorization"), "Bearer test-token")
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) == 0 {
			t.Error("expected non-empty request body")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(wantResp)
	}))
	defer ts.Close()

	c := newMQTestClientWithHTTP(t, ts.URL)

	result, err := c.UploadAudit(context.Background(), []byte(`[{"id":"id-1"},{"id":"id-2"}]`))
	if err != nil {
		t.Fatalf("UploadAudit: %v", err)
	}
	if !result.Ack {
		t.Error("Ack = false, want true")
	}
	if len(result.ConfirmedIDs) != 2 {
		t.Errorf("len(ConfirmedIDs) = %d, want 2", len(result.ConfirmedIDs))
	}
	if result.ConfirmedIDs[0] != "id-1" || result.ConfirmedIDs[1] != "id-2" {
		t.Errorf("ConfirmedIDs = %v, want [id-1, id-2]", result.ConfirmedIDs)
	}
}

func TestUploadAudit_HTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"server error"}`))
	}))
	defer ts.Close()

	c := newMQTestClientWithHTTP(t, ts.URL)

	_, err := c.UploadAudit(context.Background(), []byte(`[{"id":"1"}]`))
	if err == nil {
		t.Fatal("expected error on HTTP 500")
	}
}

func TestUploadAuditWithRetry_Success(t *testing.T) {
	wantResp := AuditBatchResponse{
		Ack:          true,
		ConfirmedIDs: []string{"id-1"},
	}
	var callCount atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(wantResp)
	}))
	defer ts.Close()

	c := newMQTestClientWithHTTP(t, ts.URL)

	result, err := c.UploadAuditWithRetry(context.Background(), []byte(`[{"id":"id-1"}]`), 3)
	if err != nil {
		t.Fatalf("UploadAuditWithRetry: %v", err)
	}
	if !result.Ack {
		t.Error("Ack = false, want true")
	}
	if callCount.Load() != 1 {
		t.Errorf("HTTP calls = %d, want 1 (no retries needed)", callCount.Load())
	}
}

func TestUploadAuditWithRetry_RetriesOnFailure(t *testing.T) {
	wantResp := AuditBatchResponse{
		Ack:          true,
		ConfirmedIDs: []string{"id-1"},
	}
	var callCount atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"temporary"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(wantResp)
	}))
	defer ts.Close()

	c := newMQTestClientWithHTTP(t, ts.URL)

	result, err := c.UploadAuditWithRetry(context.Background(), []byte(`[{"id":"id-1"}]`), 3)
	if err != nil {
		t.Fatalf("UploadAuditWithRetry: %v", err)
	}
	if !result.Ack {
		t.Error("Ack = false, want true")
	}
	if callCount.Load() != 2 {
		t.Errorf("HTTP calls = %d, want 2 (1 fail + 1 success)", callCount.Load())
	}
}
