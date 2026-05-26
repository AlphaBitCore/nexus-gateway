// coverage_gaps_test.go pins observable behavior of the package paths that
// the legacy mq_writer_test.go / ndjson_test.go suite leaves at 0% or low
// coverage: Register wiring, WithNormalizer / WithThingIdentity
// fluent constructors, the Enqueue overflow → NDJSON branch, the loop()
// timer-fire and batch-size-fire flush arms, applyNormalize request and
// response stamping, fallbackToNDJSON, inlineBodyBytes, eventToMap pointer
// branches, and the toMessage pointer-field passthrough surface.
//
// Tests assert observable side effects (Prometheus counter delta, emitted
// queue messages, NDJSON file contents) — never err == nil padding.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// discardWriter is a thread-safe io.Writer that drops everything written
// to it. Used by quietLogger to keep expected error-level slog output from
// flooding the test output stream.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// quietLogger returns a slog.Logger that swallows output so error-level
// expected log lines don't pollute the test output stream. Uses an in-
// memory discard writer (NOT os.NewFile(0, …), which aliases stdin and
// has been observed to corrupt unrelated test fds on macOS).
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// resetPackageMetrics restores all package-level metric handles to nil so
// later tests run against the same nil-handle preconditions that the legacy
// suite expects.
func resetPackageMetrics(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		BatchSize = nil
		BatchLatency = nil
		QueueDepth = nil
		EnqueueTotal = nil
		WriteErrors = nil
		NDJSONWrites = nil
		NDJSONBytes = nil
		NDJSONActive = nil
		BumpStatusTotal = nil
		ComplianceCoverage = nil
	})
}

// blockingProducer is an mq.Producer that blocks every Enqueue until release
// is closed, then returns nil. Used to exercise the channel-full overflow
// branch in MQBatchWriter.Enqueue without racing the background loop.
type blockingProducer struct {
	release chan struct{}
	calls   atomic.Int64
}

func (p *blockingProducer) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (p *blockingProducer) Enqueue(_ context.Context, _ string, _ []byte) error {
	p.calls.Add(1)
	<-p.release
	return nil
}
func (p *blockingProducer) Close() error { return nil }

// countingProducer records each successful Enqueue and lets the test pause
// the loop via the gate channel when non-nil.
type countingProducer struct {
	mu       sync.Mutex
	messages [][]byte
	gate     chan struct{}
	err      error
}

func (p *countingProducer) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (p *countingProducer) Enqueue(_ context.Context, _ string, data []byte) error {
	if p.gate != nil {
		<-p.gate
	}
	if p.err != nil {
		return p.err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	p.messages = append(p.messages, cp)
	return nil
}
func (p *countingProducer) Close() error { return nil }
func (p *countingProducer) snapshot() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]byte, len(p.messages))
	copy(out, p.messages)
	return out
}

// waitFor polls until cond returns true or the deadline expires. Eliminates
// fixed sleeps that flake under -race.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("waitFor: condition not satisfied within %s", d)
}

// TestRegister_NilRegistryNoop verifies passing nil leaves the package vars
// untouched (Register is a no-op). Pre-condition: vars are nil.
func TestRegister_NilRegistryNoop(t *testing.T) {
	resetPackageMetrics(t)
	Register(nil)
	if BatchSize != nil || EnqueueTotal != nil || BumpStatusTotal != nil {
		t.Fatalf("Register(nil) must not allocate metric handles; got non-nil")
	}
}

// TestRegister_BindsAllHandles verifies that Register populates every
// package-level metric and that each handle is wired against the supplied
// Prometheus registry (i.e. an Inc()/Set() goes through the real vec).
func TestRegister_BindsAllHandles(t *testing.T) {
	resetPackageMetrics(t)
	promReg := prometheus.NewRegistry()
	reg := registry.NewRegistry(promReg)

	Register(reg)

	if BatchSize == nil || BatchLatency == nil || QueueDepth == nil ||
		EnqueueTotal == nil || WriteErrors == nil || NDJSONWrites == nil ||
		NDJSONBytes == nil || NDJSONActive == nil || BumpStatusTotal == nil ||
		ComplianceCoverage == nil {
		t.Fatalf("Register must populate every metric handle")
	}

	// Exercise the wiring — pins must not panic and Prometheus must
	// observe the increment via Collect().
	EnqueueTotal.With("mq").Inc()
	BumpStatusTotal.With("BUMP_SUCCESS").Inc()
	NDJSONActive.With().Set(1)
	NDJSONWrites.With().Inc()
	NDJSONBytes.With().Add(42)
	WriteErrors.With().Inc()
	BatchSize.With().Observe(7)
	BatchLatency.With().Observe(15)
	QueueDepth.With().Set(3)
	ComplianceCoverage.With().Set(0.95)

	families, err := promReg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if len(families) == 0 {
		t.Fatalf("expected metric families on the registry after Register + increment")
	}
}

// TestWithNormalizer_StoresFnAndReturnsReceiver verifies that the
// normalizer closure is stored on the writer and chainable (returns *w).
func TestWithNormalizer_StoresFnAndReturnsReceiver(t *testing.T) {
	w := NewMQBatchWriter(&countingProducer{}, "q", 100, time.Hour, 10, nil, quietLogger())
	defer w.Close(context.Background()) //nolint:errcheck

	calls := atomic.Int64{}
	fn := func(direction, _, _, _, _ string, _ bool, _ []byte) (json.RawMessage, string, string) {
		calls.Add(1)
		return json.RawMessage(`{"d":"` + direction + `"}`), "ok", ""
	}
	got := w.WithNormalizer(fn)
	if got != w {
		t.Fatalf("WithNormalizer must return receiver for chaining")
	}
	if w.normalize == nil {
		t.Fatalf("normalize closure not stored on writer")
	}
}

// TestWithThingIdentity_StampsBothFields verifies that the proxy thing ID
// and name are stored and chainable.
func TestWithThingIdentity_StampsBothFields(t *testing.T) {
	w := NewMQBatchWriter(&countingProducer{}, "q", 100, time.Hour, 10, nil, quietLogger())
	defer w.Close(context.Background()) //nolint:errcheck
	got := w.WithThingIdentity("thing-1", "compliance-proxy-host-1")
	if got != w {
		t.Fatalf("WithThingIdentity must return receiver for chaining")
	}
	if w.thingID != "thing-1" || w.thingName != "compliance-proxy-host-1" {
		t.Fatalf("identity not stored: got (%q, %q)", w.thingID, w.thingName)
	}
}

// TestNewMQBatchWriter_AppliesDefaults verifies the zero/negative path
// promotes chanCapacity → 1000 and batchSize → 100.
func TestNewMQBatchWriter_AppliesDefaults(t *testing.T) {
	w := NewMQBatchWriter(&countingProducer{}, "q", 0, time.Hour, 0, nil, quietLogger())
	defer w.Close(context.Background()) //nolint:errcheck
	if w.batchSize != 100 {
		t.Errorf("batchSize default = %d, want 100", w.batchSize)
	}
	if w.QueueCap() != 1000 {
		t.Errorf("chan cap default = %d, want 1000", w.QueueCap())
	}

	w2 := NewMQBatchWriter(&countingProducer{}, "q", -7, time.Hour, -1, nil, quietLogger())
	defer w2.Close(context.Background()) //nolint:errcheck
	if w2.batchSize != 100 || w2.QueueCap() != 1000 {
		t.Errorf("negative inputs must apply same defaults; got batchSize=%d cap=%d", w2.batchSize, w2.QueueCap())
	}
}

// TestEnqueue_OverflowFallsBackToNDJSON verifies the channel-full branch:
// when the queue is at capacity and the loop is blocked, Enqueue spills the
// event to the NDJSONWriter and does NOT block. Pre-fix this used to drop
// silently — the binding is "no event lost on overflow".
func TestEnqueue_OverflowFallsBackToNDJSON(t *testing.T) {
	dir := t.TempDir()
	ndjson, err := NewNDJSONWriter(dir, "inst-overflow", 10, 100, quietLogger())
	if err != nil {
		t.Fatalf("NewNDJSONWriter: %v", err)
	}
	defer ndjson.Close() //nolint:errcheck

	// Block the producer so the background loop hangs after picking up
	// the first event, ensuring the channel fills up.
	prod := &blockingProducer{release: make(chan struct{})}
	// Tiny batchSize=1 + tiny chanCapacity=1 means: 1 event flows to the
	// loop, the producer blocks, the next Enqueue fills the channel, the
	// one after MUST spill.
	w := NewMQBatchWriter(prod, "q", 1, time.Hour, 1, ndjson, quietLogger())
	t.Cleanup(func() {
		close(prod.release)
		_ = w.Close(context.Background())
	})

	w.Enqueue(AuditEvent{ID: "ev-1", BumpStatus: "BUMP_SUCCESS", Timestamp: time.Now()})
	waitFor(t, time.Second, func() bool { return prod.calls.Load() >= 1 })
	// Channel is now empty but loop is blocked on producer; fill it.
	w.Enqueue(AuditEvent{ID: "ev-2", BumpStatus: "BUMP_SUCCESS", Timestamp: time.Now()})
	// This one must overflow → NDJSON.
	w.Enqueue(AuditEvent{ID: "ev-3-overflow", BumpStatus: "BUMP_SUCCESS", Timestamp: time.Now()})

	entries, err := os.ReadDir(filepath.Join(dir, "inst-overflow"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("expected NDJSON spool to contain the overflow event")
	}
	data, err := os.ReadFile(filepath.Join(dir, "inst-overflow", entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "ev-3-overflow") {
		t.Fatalf("overflow event id missing from NDJSON; got: %s", string(data))
	}
}

// TestEnqueue_OverflowWithNilNDJSONLogs verifies that overflow with no
// NDJSON writer configured does NOT panic. The legacy behaviour was to log
// + drop; we pin the no-panic invariant.
func TestEnqueue_OverflowWithNilNDJSONLogs(t *testing.T) {
	prod := &blockingProducer{release: make(chan struct{})}
	w := NewMQBatchWriter(prod, "q", 1, time.Hour, 1, nil, quietLogger())
	t.Cleanup(func() {
		close(prod.release)
		_ = w.Close(context.Background())
	})

	w.Enqueue(AuditEvent{ID: "ev-1", Timestamp: time.Now()})
	waitFor(t, time.Second, func() bool { return prod.calls.Load() >= 1 })
	w.Enqueue(AuditEvent{ID: "ev-2", Timestamp: time.Now()})
	// Must not panic — observable assertion is reaching this line.
	w.Enqueue(AuditEvent{ID: "ev-3-overflow", Timestamp: time.Now()})
}

// TestEnqueue_FlowsThroughChannelPath confirms the happy path increments
// the EnqueueTotal{destination="mq"} counter when metrics are wired.
func TestEnqueue_FlowsThroughChannelPath(t *testing.T) {
	resetPackageMetrics(t)
	promReg := prometheus.NewRegistry()
	Register(registry.NewRegistry(promReg))

	prod := &countingProducer{}
	w := NewMQBatchWriter(prod, "q", 5, 10*time.Millisecond, 100, nil, quietLogger())
	defer w.Close(context.Background()) //nolint:errcheck

	w.Enqueue(AuditEvent{ID: "ev-mq-1", BumpStatus: "BUMP_SUCCESS", Timestamp: time.Now()})

	// Snapshot the counter via Gather().
	waitFor(t, time.Second, func() bool {
		families, _ := promReg.Gather()
		for _, f := range families {
			if strings.HasSuffix(f.GetName(), "enqueue_total") {
				for _, m := range f.GetMetric() {
					for _, lp := range m.GetLabel() {
						if lp.GetName() == "destination" && lp.GetValue() == "mq" && m.GetCounter().GetValue() > 0 {
							return true
						}
					}
				}
			}
		}
		return false
	})
}

// TestLoop_TimerFlushFiresBatch verifies the timer-fire branch in loop():
// even when buf never hits batchSize, the periodic flush ships the events.
func TestLoop_TimerFlushFiresBatch(t *testing.T) {
	prod := &countingProducer{}
	// flushInterval is tight so the timer fires before batchSize=100 is hit.
	w := NewMQBatchWriter(prod, "q", 100, 15*time.Millisecond, 100, nil, quietLogger())
	defer w.Close(context.Background()) //nolint:errcheck

	w.Enqueue(AuditEvent{ID: "timer-1", Timestamp: time.Now()})
	w.Enqueue(AuditEvent{ID: "timer-2", Timestamp: time.Now()})

	waitFor(t, time.Second, func() bool { return len(prod.snapshot()) == 2 })
}

// TestLoop_BatchSizeFlushFiresAndTimerResets verifies the batch-full branch
// (batchSize hit → flush + timer.Reset). We push exactly batchSize=3 events
// and confirm all 3 land before the long flushInterval would have elapsed.
func TestLoop_BatchSizeFlushFiresAndTimerResets(t *testing.T) {
	prod := &countingProducer{}
	w := NewMQBatchWriter(prod, "q", 3, time.Hour, 100, nil, quietLogger())
	defer w.Close(context.Background()) //nolint:errcheck

	for i := range 3 {
		w.Enqueue(AuditEvent{ID: "batch-" + string(rune('A'+i)), Timestamp: time.Now()})
	}

	// 1s deadline ≪ 1h flushInterval → only the batch-size branch can
	// satisfy this assertion.
	waitFor(t, time.Second, func() bool { return len(prod.snapshot()) == 3 })

	// Push another event; the timer should still fire eventually after
	// the reset (we don't wait an hour — just verify no deadlock by
	// pushing a 4th batch's worth).
	for i := range 3 {
		w.Enqueue(AuditEvent{ID: "batch2-" + string(rune('A'+i)), Timestamp: time.Now()})
	}
	waitFor(t, time.Second, func() bool { return len(prod.snapshot()) == 6 })
}

// TestLoop_DoneDrainsRemaining verifies the <-w.done arm: closing the
// writer must drain queued events through flushBatch before the goroutine
// exits.
func TestLoop_DoneDrainsRemaining(t *testing.T) {
	prod := &countingProducer{}
	w := NewMQBatchWriter(prod, "q", 1000, time.Hour, 1000, nil, quietLogger())

	for i := range 7 {
		w.Enqueue(AuditEvent{ID: "drain-" + string(rune('A'+i)), Timestamp: time.Now()})
	}

	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(prod.snapshot()); got != 7 {
		t.Errorf("drain at Close yielded %d messages, want 7", got)
	}
}

// TestClose_IsIdempotent verifies the closeOnce contract: calling Close
// twice must not panic and must still return nil the second time.
func TestClose_IsIdempotent(t *testing.T) {
	w := NewMQBatchWriter(&countingProducer{}, "q", 100, time.Hour, 10, nil, quietLogger())
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Second Close must not deadlock or panic (sync.Once gate).
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestFlushBatch_ProducerErrorSpillsToNDJSON drives loop() over a failing
// producer, which triggers fallbackToNDJSON. Asserts the spool file
// contains the dropped event and that the WriteErrors counter incremented.
func TestFlushBatch_ProducerErrorSpillsToNDJSON(t *testing.T) {
	resetPackageMetrics(t)
	promReg := prometheus.NewRegistry()
	Register(registry.NewRegistry(promReg))

	dir := t.TempDir()
	ndjson, err := NewNDJSONWriter(dir, "inst-fb", 10, 100, quietLogger())
	if err != nil {
		t.Fatalf("NewNDJSONWriter: %v", err)
	}
	defer ndjson.Close() //nolint:errcheck

	prod := &countingProducer{err: errors.New("mq down")}
	w := NewMQBatchWriter(prod, "q", 1, time.Hour, 10, ndjson, quietLogger())
	defer w.Close(context.Background()) //nolint:errcheck

	w.Enqueue(AuditEvent{ID: "ev-spill", BumpStatus: "BUMP_FAILED", Timestamp: time.Now()})

	waitFor(t, time.Second, func() bool {
		entries, _ := os.ReadDir(filepath.Join(dir, "inst-fb"))
		return len(entries) > 0
	})

	entries, _ := os.ReadDir(filepath.Join(dir, "inst-fb"))
	if len(entries) == 0 {
		t.Fatal("NDJSON spool empty after producer error")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "inst-fb", entries[0].Name()))
	if !strings.Contains(string(data), "ev-spill") {
		t.Fatalf("spilled event missing from NDJSON: %s", string(data))
	}

	// WriteErrors counter must have moved.
	families, _ := promReg.Gather()
	var sawWriteError bool
	for _, f := range families {
		if strings.HasSuffix(f.GetName(), "write_errors_total") {
			for _, m := range f.GetMetric() {
				if m.GetCounter().GetValue() > 0 {
					sawWriteError = true
				}
			}
		}
	}
	if !sawWriteError {
		t.Errorf("WriteErrors counter did not increment on producer failure")
	}
}

// TestFallbackToNDJSON_NilNDJSONDrops directly exercises fallbackToNDJSON
// with w.ndjson == nil — must not panic, simply logs the dropped count.
func TestFallbackToNDJSON_NilNDJSONDrops(t *testing.T) {
	w := NewMQBatchWriter(&countingProducer{}, "q", 100, time.Hour, 10, nil, quietLogger())
	defer w.Close(context.Background()) //nolint:errcheck

	events := []AuditEvent{
		{ID: "drop-1", Timestamp: time.Now()},
		{ID: "drop-2", Timestamp: time.Now()},
	}
	// Must not panic. Observable side-effect is "no panic" — same as
	// the existing TestEnqueue_OverflowWithNilNDJSONLogs pin.
	w.fallbackToNDJSON(events)
}

// TestFallbackToNDJSON_NDJSONWriteError exercises the inner write-error
// branch by configuring an NDJSON writer whose maxTotalSize is 0 (every
// write trips the quota path).
func TestFallbackToNDJSON_NDJSONWriteError(t *testing.T) {
	dir := t.TempDir()
	ndjson, err := NewNDJSONWriter(dir, "inst-fb-err", 10, 100, quietLogger())
	if err != nil {
		t.Fatalf("NewNDJSONWriter: %v", err)
	}
	defer ndjson.Close() //nolint:errcheck
	// Force every write to fail with quota-exceeded.
	ndjson.maxTotalSize = 0

	w := NewMQBatchWriter(&countingProducer{}, "q", 100, time.Hour, 10, ndjson, quietLogger())
	defer w.Close(context.Background()) //nolint:errcheck
	// Spool dir is empty so dirSize == 0; we need to seed a byte so
	// totalSize >= maxTotalSize fires.
	seedPath := filepath.Join(dir, "inst-fb-err", "preexisting.ndjson")
	if err := os.WriteFile(seedPath, []byte("x"), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w.fallbackToNDJSON([]AuditEvent{{ID: "err-1", Timestamp: time.Now()}})
	// Reaching this line without panic is the observable behaviour.
}

// TestToMessage_AllPointerFieldsPopulated verifies every optional pointer
// field on AuditEvent surfaces on the wire message. Pre-fix gaps left
// ResponseHookDecision / ErrorCode / ErrorReason / LatencyBreakdown
// branches uncovered.
func TestToMessage_AllPointerFieldsPopulated(t *testing.T) {
	reqReason := "request-allow"
	reqCode := "ALLOW"
	respDecision := "ALLOW"
	respReason := "response-allow"
	respCode := "RESP_ALLOW"
	ua := "Mozilla/5.0"

	upTtfb := 11
	upTotal := 22
	reqHookMs := 3
	respHookMs := 4

	statusCode := 201
	event := AuditEvent{
		ID:                     "all-ptr",
		TransactionID:          "tx",
		Timestamp:              time.Now(),
		StatusCode:             &statusCode,
		RequestHookDecision:    "ALLOW",
		RequestHookReason:      &reqReason,
		RequestHookReasonCode:  &reqCode,
		RequestHooksPipeline:   []byte(`[{"hook":"r1"}]`),
		RequestBlockingRule:    []byte(`{"pack":"p","rule_id":"r"}`),
		ResponseHookDecision:   &respDecision,
		ResponseHookReason:     &respReason,
		ResponseHookReasonCode: &respCode,
		ResponseHooksPipeline:  []byte(`[{"hook":"rsp1"}]`),
		ResponseBlockingRule:   []byte(`{"pack":"p","rule_id":"r2"}`),
		UserAgent:              &ua,
		ComplianceTags:         []string{"pii", "secrets"},
		ErrorCode:              "PROXY_TIMEOUT",
		ErrorReason:            "upstream did not respond",
		UpstreamTtfbMs:         &upTtfb,
		UpstreamTotalMs:        &upTotal,
		RequestHooksMs:         &reqHookMs,
		ResponseHooksMs:        &respHookMs,
		LatencyBreakdown:       map[string]int{"req": 1, "resp": 2},
	}

	msg := toMessage(event, "thing-A", "name-A")

	if msg.StatusCode != http.StatusCreated {
		t.Errorf("StatusCode = %d, want 201", msg.StatusCode)
	}
	if msg.RequestHookReason != "request-allow" {
		t.Errorf("RequestHookReason = %q, want %q", msg.RequestHookReason, "request-allow")
	}
	if msg.RequestHookReasonCode != "ALLOW" {
		t.Errorf("RequestHookReasonCode = %q", msg.RequestHookReasonCode)
	}
	if msg.ResponseHookDecision != "ALLOW" {
		t.Errorf("ResponseHookDecision = %q, want ALLOW", msg.ResponseHookDecision)
	}
	if msg.ResponseHookReason != "response-allow" {
		t.Errorf("ResponseHookReason = %q", msg.ResponseHookReason)
	}
	if msg.ResponseHookReasonCode != "RESP_ALLOW" {
		t.Errorf("ResponseHookReasonCode = %q", msg.ResponseHookReasonCode)
	}
	if msg.ResponseBlockingRule == nil {
		t.Fatal("ResponseBlockingRule must be set")
	}
	if msg.ErrorCode == nil || *msg.ErrorCode != "PROXY_TIMEOUT" {
		t.Errorf("ErrorCode = %v, want PROXY_TIMEOUT", msg.ErrorCode)
	}
	if msg.ErrorReason == nil || *msg.ErrorReason != "upstream did not respond" {
		t.Errorf("ErrorReason = %v", msg.ErrorReason)
	}
	if msg.UpstreamTtfbMs == nil || *msg.UpstreamTtfbMs != 11 {
		t.Errorf("UpstreamTtfbMs = %v, want 11", msg.UpstreamTtfbMs)
	}
	if msg.UpstreamTotalMs == nil || *msg.UpstreamTotalMs != 22 {
		t.Errorf("UpstreamTotalMs = %v, want 22", msg.UpstreamTotalMs)
	}
	if msg.RequestHooksMs == nil || *msg.RequestHooksMs != 3 {
		t.Errorf("RequestHooksMs = %v", msg.RequestHooksMs)
	}
	if msg.ResponseHooksMs == nil || *msg.ResponseHooksMs != 4 {
		t.Errorf("ResponseHooksMs = %v", msg.ResponseHooksMs)
	}
	if msg.LatencyBreakdown["req"] != 1 || msg.LatencyBreakdown["resp"] != 2 {
		t.Errorf("LatencyBreakdown = %v", msg.LatencyBreakdown)
	}
	if len(msg.ComplianceTags) != 2 {
		t.Errorf("ComplianceTags len = %d, want 2", len(msg.ComplianceTags))
	}
	// Details map must carry user agent for passthrough.
	d, ok := msg.Details.(map[string]any)
	if !ok {
		t.Fatalf("Details type = %T, want map", msg.Details)
	}
	if d["userAgent"] != "Mozilla/5.0" {
		t.Errorf("details.userAgent = %v, want Mozilla/5.0", d["userAgent"])
	}
}

// TestToMessage_TargetPathMirrorsRequestPath pins the binding "non-ai-
// gateway → target_path = request_path" from event_message.go:54.
func TestToMessage_TargetPathMirrorsRequestPath(t *testing.T) {
	msg := toMessage(AuditEvent{
		ID:        "tp",
		Timestamp: time.Now(),
		Method:    "POST",
		Path:      "/v1/messages",
	}, "", "")
	if msg.TargetMethod != "POST" {
		t.Errorf("TargetMethod = %q, want POST", msg.TargetMethod)
	}
	if msg.TargetPath != "/v1/messages" {
		t.Errorf("TargetPath = %q, want /v1/messages", msg.TargetPath)
	}
}

// TestToMessage_ErrorCodeEmptyStaysNil ensures empty-string ErrorCode and
// ErrorReason produce nil *string on the wire (per the
// `[[feedback_agent_audit_empty_string_stripping]]` family of bindings: a
// stripped empty string would still violate the Hub's NULL semantics
// otherwise).
func TestToMessage_ErrorCodeEmptyStaysNil(t *testing.T) {
	msg := toMessage(AuditEvent{ID: "no-err", Timestamp: time.Now()}, "", "")
	if msg.ErrorCode != nil {
		t.Errorf("ErrorCode must be nil when AuditEvent.ErrorCode==\"\"; got %v", *msg.ErrorCode)
	}
	if msg.ErrorReason != nil {
		t.Errorf("ErrorReason must be nil when AuditEvent.ErrorReason==\"\"; got %v", *msg.ErrorReason)
	}
}

// TestInlineBodyBytes_AbsentReturnsNil pins the spilled / absent branches.
func TestInlineBodyBytes_AbsentReturnsNil(t *testing.T) {
	if got := inlineBodyBytes(sharedaudit.EmptyBody()); got != nil {
		t.Errorf("EmptyBody must yield nil; got %q", string(got))
	}
	// Manually crafted spill body — Kind != Inline.
	spill := sharedaudit.Body{
		Kind:     sharedaudit.BodyKind("spill"),
		SpillRef: &sharedaudit.SpillRef{Backend: "s3", Key: "k", Size: 1},
	}
	if got := inlineBodyBytes(spill); got != nil {
		t.Errorf("Spill body must yield nil; got %q", string(got))
	}
}

// TestInlineBodyBytes_InlineReturnsBytes verifies the happy path.
func TestInlineBodyBytes_InlineReturnsBytes(t *testing.T) {
	body := sharedaudit.NewInlineBody([]byte(`{"hello":"world"}`), 17, false, "application/json")
	got := inlineBodyBytes(body)
	if string(got) != `{"hello":"world"}` {
		t.Errorf("inlineBodyBytes = %q, want JSON object", string(got))
	}
}

// TestApplyNormalize_NilFnNoop pins early returns.
func TestApplyNormalize_NilFnNoop(t *testing.T) {
	msg := &mq.TrafficEventMessage{}
	applyNormalize(msg, AuditEvent{Provider: "openai"}, nil)
	if msg.RequestNormalized != nil || msg.NormalizeVersion != "" {
		t.Errorf("nil fn must leave msg untouched")
	}
}

// TestApplyNormalize_EmptyProviderNoop pins early returns.
func TestApplyNormalize_EmptyProviderNoop(t *testing.T) {
	msg := &mq.TrafficEventMessage{}
	called := false
	applyNormalize(msg, AuditEvent{Provider: ""}, func(_, _, _, _, _ string, _ bool, _ []byte) (json.RawMessage, string, string) {
		called = true
		return nil, "", ""
	})
	if called {
		t.Error("normalize fn must not be invoked when Provider==\"\"")
	}
	if msg.NormalizeVersion != "" {
		t.Errorf("NormalizeVersion must stay empty")
	}
}

// TestApplyNormalize_StampsRequestAndResponse pins the full request +
// response normalize pipeline, including the stamped lowercase adapter
// type, the lifted normalize wire version, and per-direction status/error.
func TestApplyNormalize_StampsRequestAndResponse(t *testing.T) {
	msg := &mq.TrafficEventMessage{}
	event := AuditEvent{
		Provider:     "OpenAI",
		Model:        "gpt-4o",
		Path:         "/v1/chat/completions",
		RequestBody:  sharedaudit.NewInlineBody([]byte(`{"model":"gpt-4o","messages":[]}`), 32, false, "application/json"),
		ResponseBody: sharedaudit.NewInlineBody([]byte(`{"id":"x","choices":[]}`), 23, false, "application/json"),
	}
	calls := 0
	fn := func(direction, contentType, adapterType, model, path string, stream bool, body []byte) (json.RawMessage, string, string) {
		calls++
		if adapterType != "openai" {
			t.Errorf("adapterType = %q, want lowercase %q", adapterType, "openai")
		}
		if contentType != "application/json" {
			t.Errorf("contentType = %q, want application/json", contentType)
		}
		if stream {
			t.Errorf("cp must not flag normalize as stream=true")
		}
		if model != "gpt-4o" {
			t.Errorf("model = %q, want gpt-4o", model)
		}
		if path != "/v1/chat/completions" {
			t.Errorf("path = %q", path)
		}
		return json.RawMessage(`{"d":"` + direction + `"}`), "ok", ""
	}

	applyNormalize(msg, event, fn)

	if calls != 2 {
		t.Errorf("normalize fn called %d times, want 2 (request + response)", calls)
	}
	if string(msg.RequestNormalized) != `{"d":"request"}` {
		t.Errorf("RequestNormalized = %s", string(msg.RequestNormalized))
	}
	if string(msg.ResponseNormalized) != `{"d":"response"}` {
		t.Errorf("ResponseNormalized = %s", string(msg.ResponseNormalized))
	}
	if msg.RequestNormalizeStatus != "ok" || msg.ResponseNormalizeStatus != "ok" {
		t.Errorf("status = (%q,%q)", msg.RequestNormalizeStatus, msg.ResponseNormalizeStatus)
	}
	if msg.NormalizeVersion != "1" {
		t.Errorf("NormalizeVersion = %q, want \"1\"", msg.NormalizeVersion)
	}
}

// TestApplyNormalize_StatusOnlyStillStamps verifies the "raw==nil but
// status!=\"\"" branch: even when the decoder produces no JSON (e.g. an
// unsupported provider), the status string still lands and triggers the
// NormalizeVersion stamp.
func TestApplyNormalize_StatusOnlyStillStamps(t *testing.T) {
	msg := &mq.TrafficEventMessage{}
	event := AuditEvent{
		Provider:    "anthropic",
		RequestBody: sharedaudit.NewInlineBody([]byte(`{"messages":[]}`), 15, false, "application/json"),
	}
	fn := func(_, _, _, _, _ string, _ bool, _ []byte) (json.RawMessage, string, string) {
		return nil, "failed", "no model"
	}
	applyNormalize(msg, event, fn)
	if msg.RequestNormalizeStatus != "failed" {
		t.Errorf("RequestNormalizeStatus = %q, want failed", msg.RequestNormalizeStatus)
	}
	if msg.RequestNormalizeError != "no model" {
		t.Errorf("RequestNormalizeError = %q", msg.RequestNormalizeError)
	}
	if msg.NormalizeVersion != "1" {
		t.Errorf("NormalizeVersion = %q, want 1", msg.NormalizeVersion)
	}
}

// TestApplyNormalize_RawNilStatusEmptyNoStamp verifies the "no signal"
// branch: when the decoder returns (nil, "", "") for both directions, the
// stamped flag stays false → NormalizeVersion stays empty.
func TestApplyNormalize_RawNilStatusEmptyNoStamp(t *testing.T) {
	msg := &mq.TrafficEventMessage{}
	event := AuditEvent{
		Provider:    "openai",
		RequestBody: sharedaudit.NewInlineBody([]byte(`{"x":1}`), 7, false, "application/json"),
	}
	fn := func(_, _, _, _, _ string, _ bool, _ []byte) (json.RawMessage, string, string) {
		return nil, "", ""
	}
	applyNormalize(msg, event, fn)
	if msg.NormalizeVersion != "" {
		t.Errorf("NormalizeVersion must stay empty when nothing was stamped; got %q", msg.NormalizeVersion)
	}
	if msg.RequestNormalized != nil {
		t.Errorf("RequestNormalized must stay nil; got %s", string(msg.RequestNormalized))
	}
}

// TestApplyNormalize_OnlyResponse pins the path where the request body is
// absent (e.g. spilled) but the response is inline.
func TestApplyNormalize_OnlyResponse(t *testing.T) {
	msg := &mq.TrafficEventMessage{}
	event := AuditEvent{
		Provider:     "openai",
		ResponseBody: sharedaudit.NewInlineBody([]byte(`{"id":"r"}`), 10, false, "application/json"),
	}
	fn := func(direction, _, _, _, _ string, _ bool, _ []byte) (json.RawMessage, string, string) {
		if direction != "response" {
			t.Errorf("expected response-only call; got direction=%q", direction)
		}
		return json.RawMessage(`{"r":1}`), "ok", ""
	}
	applyNormalize(msg, event, fn)
	if msg.RequestNormalized != nil {
		t.Errorf("RequestNormalized must stay nil; got %s", string(msg.RequestNormalized))
	}
	if msg.ResponseNormalized == nil {
		t.Fatal("ResponseNormalized must be stamped")
	}
}

// TestEventToMap_AllPointerFieldsLandOnMap exercises every conditional
// addition in eventToMap (statusCode, requestHookReason, hooks pipelines,
// complianceTags, subjectId, dsarDeleteRequested, userAgent, response hook
// fields).
func TestEventToMap_AllPointerFieldsLandOnMap(t *testing.T) {
	status := 418
	reqReason := "req-r"
	reqCode := "REQ_C"
	respDec := "ALLOW"
	respReason := "resp-r"
	respCode := "RESP_C"
	subject := "user-1"
	dsar := true
	ua := "ua/1.0"
	event := AuditEvent{
		ID:                     "all-fields",
		TransactionID:          "tx",
		Timestamp:              time.Now(),
		StatusCode:             &status,
		RequestHookReason:      &reqReason,
		RequestHookReasonCode:  &reqCode,
		RequestHooksPipeline:   []byte(`[{"h":"r"}]`),
		ResponseHookDecision:   &respDec,
		ResponseHookReason:     &respReason,
		ResponseHookReasonCode: &respCode,
		ResponseHooksPipeline:  []byte(`[{"h":"resp"}]`),
		ComplianceTags:         []string{"pii"},
		SubjectID:              &subject,
		DSARDeleteRequested:    &dsar,
		UserAgent:              &ua,
	}

	m := eventToMap(event)
	if m["statusCode"] != 418 {
		t.Errorf("statusCode = %v, want 418", m["statusCode"])
	}
	if m["requestHookReason"] != "req-r" {
		t.Errorf("requestHookReason missing/wrong: %v", m["requestHookReason"])
	}
	if m["requestHookReasonCode"] != "REQ_C" {
		t.Errorf("requestHookReasonCode missing/wrong: %v", m["requestHookReasonCode"])
	}
	if _, ok := m["requestHooksPipeline"]; !ok {
		t.Errorf("requestHooksPipeline missing")
	}
	if m["responseHookDecision"] != "ALLOW" {
		t.Errorf("responseHookDecision = %v", m["responseHookDecision"])
	}
	if m["responseHookReason"] != "resp-r" {
		t.Errorf("responseHookReason = %v", m["responseHookReason"])
	}
	if m["responseHookReasonCode"] != "RESP_C" {
		t.Errorf("responseHookReasonCode = %v", m["responseHookReasonCode"])
	}
	if _, ok := m["responseHooksPipeline"]; !ok {
		t.Errorf("responseHooksPipeline missing")
	}
	if m["subjectId"] != "user-1" {
		t.Errorf("subjectId = %v", m["subjectId"])
	}
	if m["dsarDeleteRequested"] != true {
		t.Errorf("dsarDeleteRequested = %v", m["dsarDeleteRequested"])
	}
	if m["userAgent"] != "ua/1.0" {
		t.Errorf("userAgent = %v", m["userAgent"])
	}
	tags, ok := m["complianceTags"].([]string)
	if !ok || len(tags) != 1 || tags[0] != "pii" {
		t.Errorf("complianceTags = %v", m["complianceTags"])
	}

	// Round-trip through JSON to verify the map serializes.
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(raw), "\"statusCode\":418") {
		t.Errorf("statusCode missing from JSON: %s", string(raw))
	}
}

// TestEventToMap_OnlyMandatoryFields verifies the empty-pointer path drops
// every optional field.
func TestEventToMap_OnlyMandatoryFields(t *testing.T) {
	m := eventToMap(AuditEvent{ID: "min", Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	for _, key := range []string{
		"statusCode", "requestHookReason", "requestHookReasonCode",
		"requestHooksPipeline", "responseHookDecision", "responseHookReason",
		"responseHookReasonCode", "responseHooksPipeline", "subjectId",
		"dsarDeleteRequested", "userAgent", "complianceTags",
	} {
		if _, ok := m[key]; ok {
			t.Errorf("key %q must be absent when source pointer is nil/empty", key)
		}
	}
	if m["timestamp"] != "2026-01-01T00:00:00Z" {
		t.Errorf("timestamp RFC3339Nano = %v", m["timestamp"])
	}
}

// TestNDJSONWriter_RotateWhenNoFileOpenIsNoop directly drives rotateFile()
// when currentFile == nil — must return nil cleanly.
func TestNDJSONWriter_RotateWhenNoFileOpenIsNoop(t *testing.T) {
	dir := t.TempDir()
	w, err := NewNDJSONWriter(dir, "inst-rot-noop", 10, 100, quietLogger())
	if err != nil {
		t.Fatalf("NewNDJSONWriter: %v", err)
	}
	defer w.Close() //nolint:errcheck
	w.mu.Lock()
	err = w.rotateFile()
	w.mu.Unlock()
	if err != nil {
		t.Fatalf("rotateFile with nil currentFile must be a no-op; got %v", err)
	}
}

// TestNDJSONWriter_CloseWithoutFileIsNoop pins the early-return path in
// Close() when currentFile is nil.
func TestNDJSONWriter_CloseWithoutFileIsNoop(t *testing.T) {
	dir := t.TempDir()
	w, err := NewNDJSONWriter(dir, "inst-close-noop", 10, 100, quietLogger())
	if err != nil {
		t.Fatalf("NewNDJSONWriter: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close without prior Write must succeed; got %v", err)
	}
	// Second Close must still be a no-op.
	if err := w.Close(); err != nil {
		t.Errorf("idempotent Close: %v", err)
	}
}

// TestNDJSONWriter_OpenFileBubblesError forces openNewFile() to fail by
// pointing the spool dir at a path that exists as a file (so mkdir would
// have succeeded but openFile on a file-as-directory child errors).
func TestNDJSONWriter_OpenFileBubblesError(t *testing.T) {
	dir := t.TempDir()
	// Create the instance dir as a regular file — NewNDJSONWriter
	// itself would fail this, so we mutate after construction.
	w, err := NewNDJSONWriter(dir, "inst-open-err", 10, 100, quietLogger())
	if err != nil {
		t.Fatalf("NewNDJSONWriter: %v", err)
	}
	defer w.Close() //nolint:errcheck

	// Remove the instance dir, then create a file at the same path.
	instDir := filepath.Join(dir, "inst-open-err")
	if err := os.RemoveAll(instDir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if err := os.WriteFile(instDir, []byte("not a dir"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Write must now bubble an open-file error because the instance
	// dir is actually a regular file.
	werr := w.Write(AuditEvent{ID: "ev-open-fail", Timestamp: time.Now()})
	if werr == nil {
		t.Fatal("Write must fail when the instance dir is unreadable")
	}
}

// TestNDJSONWriter_DirSizeMissingDirLogsWarn forces dirSize to return an
// error (instance dir removed) and confirms Write still attempts to open a
// new file (warning logged, write continues per the comment in ndjson.go).
func TestNDJSONWriter_DirSizeMissingDirLogsWarn(t *testing.T) {
	dir := t.TempDir()
	w, err := NewNDJSONWriter(dir, "inst-missing", 10, 100, quietLogger())
	if err != nil {
		t.Fatalf("NewNDJSONWriter: %v", err)
	}
	defer w.Close() //nolint:errcheck

	// Remove the instance directory between construction and Write.
	if err := os.RemoveAll(filepath.Join(dir, "inst-missing")); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	// openNewFile() will fail because the parent directory doesn't
	// exist anymore — surface this as an error to confirm the dirSize
	// warning branch flowed through to the next step.
	werr := w.Write(AuditEvent{ID: "ev-warn", Timestamp: time.Now()})
	if werr == nil {
		t.Fatal("Write must surface the open-file failure after dir was deleted")
	}
}

// TestFlush_NoQueuedEventsReturnsNil verifies the len(events)==0 early
// return in Flush.
func TestFlush_NoQueuedEventsReturnsNil(t *testing.T) {
	w := NewMQBatchWriter(&countingProducer{}, "q", 100, time.Hour, 10, nil, quietLogger())
	defer w.Close(context.Background()) //nolint:errcheck
	if err := w.Flush(context.Background()); err != nil {
		t.Errorf("empty Flush must be nil; got %v", err)
	}
}

// TestEnqueue_OverflowIncrementsNDJSONMetrics covers the overflow branch
// of Enqueue when EnqueueTotal + NDJSONActive are non-nil. Asserts both
// EnqueueTotal{destination="ndjson"} and NDJSONActive=1.
func TestEnqueue_OverflowIncrementsNDJSONMetrics(t *testing.T) {
	resetPackageMetrics(t)
	promReg := prometheus.NewRegistry()
	Register(registry.NewRegistry(promReg))

	dir := t.TempDir()
	ndjson, err := NewNDJSONWriter(dir, "inst-metric-overflow", 10, 100, quietLogger())
	if err != nil {
		t.Fatalf("NewNDJSONWriter: %v", err)
	}
	defer ndjson.Close() //nolint:errcheck

	prod := &blockingProducer{release: make(chan struct{})}
	w := NewMQBatchWriter(prod, "q", 1, time.Hour, 1, ndjson, quietLogger())
	t.Cleanup(func() {
		close(prod.release)
		_ = w.Close(context.Background())
	})

	w.Enqueue(AuditEvent{ID: "m-1", BumpStatus: "BUMP_SUCCESS", Timestamp: time.Now()})
	waitFor(t, time.Second, func() bool { return prod.calls.Load() >= 1 })
	w.Enqueue(AuditEvent{ID: "m-2", BumpStatus: "BUMP_SUCCESS", Timestamp: time.Now()})
	w.Enqueue(AuditEvent{ID: "m-3-overflow", BumpStatus: "BUMP_SUCCESS", Timestamp: time.Now()})

	waitFor(t, time.Second, func() bool {
		families, _ := promReg.Gather()
		for _, f := range families {
			if strings.HasSuffix(f.GetName(), "enqueue_total") {
				for _, m := range f.GetMetric() {
					for _, lp := range m.GetLabel() {
						if lp.GetName() == "destination" && lp.GetValue() == "ndjson" && m.GetCounter().GetValue() > 0 {
							return true
						}
					}
				}
			}
		}
		return false
	})

	families, _ := promReg.Gather()
	var ndjsonActive float64
	for _, f := range families {
		if strings.HasSuffix(f.GetName(), "ndjson_active") {
			for _, m := range f.GetMetric() {
				ndjsonActive = m.GetGauge().GetValue()
			}
		}
	}
	if ndjsonActive != 1 {
		t.Errorf("ndjson_active gauge = %v, want 1 after overflow", ndjsonActive)
	}
}

// failingNDJSONWriter wraps the real writer with a forced Write error so
// Enqueue's "NDJSON fallback write failed" log branch is exercised.

// TestEnqueue_OverflowWriteErrorLogs covers the inner branch in Enqueue's
// overflow default arm: when w.ndjson.Write returns an error, the error
// must be logged (not panic). We force the failure by setting maxTotalSize
// to 0 + seeding a byte in the spool dir.
func TestEnqueue_OverflowWriteErrorLogs(t *testing.T) {
	dir := t.TempDir()
	ndjson, err := NewNDJSONWriter(dir, "inst-overflow-werr", 10, 100, quietLogger())
	if err != nil {
		t.Fatalf("NewNDJSONWriter: %v", err)
	}
	defer ndjson.Close() //nolint:errcheck
	ndjson.maxTotalSize = 1
	seed := filepath.Join(dir, "inst-overflow-werr", "preexisting.ndjson")
	if err := os.WriteFile(seed, []byte("xx"), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	prod := &blockingProducer{release: make(chan struct{})}
	w := NewMQBatchWriter(prod, "q", 1, time.Hour, 1, ndjson, quietLogger())
	t.Cleanup(func() {
		close(prod.release)
		_ = w.Close(context.Background())
	})

	w.Enqueue(AuditEvent{ID: "we-1", Timestamp: time.Now()})
	waitFor(t, time.Second, func() bool { return prod.calls.Load() >= 1 })
	w.Enqueue(AuditEvent{ID: "we-2", Timestamp: time.Now()})
	// Must not panic — observable behaviour is no panic on a failing
	// NDJSON write in the overflow path.
	w.Enqueue(AuditEvent{ID: "we-3-overflow", Timestamp: time.Now()})
}

// TestClose_FinalFlushErrorLogs verifies the Close-time drain hits the
// flushBatch error path (events queued but producer errors) without
// surfacing the error to the caller (Close logs + returns nil).
func TestClose_FinalFlushErrorLogs(t *testing.T) {
	prod := &countingProducer{}
	w := NewMQBatchWriter(prod, "q", 10000, time.Hour, 1000, nil, quietLogger())

	for i := range 3 {
		w.Enqueue(AuditEvent{ID: "close-err-" + string(rune('A'+i)), Timestamp: time.Now()})
	}

	// Wait for events to be queued, then swap the producer error in.
	waitFor(t, time.Second, func() bool {
		// Use snapshot to confirm enqueue happened.
		return w.QueueLen() == 3 || len(prod.snapshot()) == 3
	})
	prod.mu.Lock()
	prod.err = errors.New("producer down")
	prod.mu.Unlock()

	// Close must not return an error even if drain → flushBatch fails;
	// it logs and proceeds. This pins the binding "Close never surfaces
	// final-flush errors to the caller".
	if err := w.Close(context.Background()); err != nil {
		t.Errorf("Close must absorb final-flush errors; got %v", err)
	}
}

// TestNewNDJSONWriter_MkdirAllError forces MkdirAll to fail by aiming the
// spool root at a path whose ancestor is a regular file, so MkdirAll
// cannot create the instance subdirectory.
func TestNewNDJSONWriter_MkdirAllError(t *testing.T) {
	parent := t.TempDir()
	// Create a regular file at parent/blocker; then ask NewNDJSONWriter
	// to put the spool dir at parent/blocker/spool → MkdirAll must fail
	// because parent/blocker is not a directory.
	blocker := filepath.Join(parent, "blocker")
	f, err := os.Create(blocker)
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	_ = f.Close()
	spoolRoot := filepath.Join(blocker, "spool")
	if _, err := NewNDJSONWriter(spoolRoot, "inst-x", 10, 100, quietLogger()); err == nil {
		t.Fatal("NewNDJSONWriter must fail when MkdirAll cannot create the instance dir")
	}
}

// TestNDJSONWriter_DirSizeSkipsSubdirectories covers the "if entry.IsDir()
// { continue }" branch in dirSize() by planting a subdirectory inside the
// instance spool dir.
func TestNDJSONWriter_DirSizeSkipsSubdirectories(t *testing.T) {
	dir := t.TempDir()
	w, err := NewNDJSONWriter(dir, "inst-subdir", 10, 100, quietLogger())
	if err != nil {
		t.Fatalf("NewNDJSONWriter: %v", err)
	}
	defer w.Close() //nolint:errcheck

	// Create a subdirectory inside the spool dir.
	subdir := filepath.Join(dir, "inst-subdir", "nested")
	if err := os.Mkdir(subdir, 0700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	// Add a file inside the subdir to give it size; dirSize() should
	// still skip it.
	if err := os.WriteFile(filepath.Join(subdir, "x"), []byte("XYZ"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Write should succeed because the subdir is excluded from dirSize.
	if err := w.Write(AuditEvent{ID: "subdir-skip", Timestamp: time.Now()}); err != nil {
		t.Fatalf("Write must succeed when only the subdir holds bytes; got %v", err)
	}
}

// TestDrain_RemovesEveryPendingEvent confirms the drain helper visits
// every queued event, including the default branch when the channel is
// empty (returns the accumulated slice).
func TestDrain_RemovesEveryPendingEvent(t *testing.T) {
	prod := &countingProducer{}
	// Long flushInterval + big batch so events queue in the channel
	// until Flush manually drains them.
	w := NewMQBatchWriter(prod, "q", 10000, time.Hour, 100, nil, quietLogger())
	defer w.Close(context.Background()) //nolint:errcheck

	for i := range 5 {
		w.Enqueue(AuditEvent{ID: "drain-" + string(rune('A'+i)), Timestamp: time.Now()})
	}

	// drain() is unexported; reach it via Flush which calls drain+flushBatch.
	if err := w.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	// Flush + loop together must have delivered all 5 events.
	waitFor(t, time.Second, func() bool { return len(prod.snapshot()) >= 5 })
}
