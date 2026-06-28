package audit

import (
	"context"
	"github.com/goccy/go-json"
	"net/http"
	"sync"
	"testing"
	"time"

	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

type memProducer struct {
	mu       sync.Mutex
	messages []memMsg
	failAll  bool
	// blockUntil, when non-nil, makes Enqueue wait for the channel to be
	// closed before it returns. Used to exercise Flush's ctx-cancellation
	// branches: the loop is wedged inside flushBatch, so a subsequent
	// Flush observes its own ctx deadline before the ack arrives.
	blockUntil chan struct{}
	// entered signals (via close) the first time Enqueue is called while
	// blockUntil is set, so tests can synchronise with the loop being
	// wedged before tripping the next assertion.
	entered chan struct{}
}

type memMsg struct {
	queue string
	data  []byte
}

func (p *memProducer) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (p *memProducer) Enqueue(_ context.Context, queue string, data []byte) error {
	p.mu.Lock()
	block := p.blockUntil
	entered := p.entered
	failAll := p.failAll
	p.mu.Unlock()

	if block != nil {
		if entered != nil {
			select {
			case <-entered:
			default:
				close(entered)
			}
		}
		<-block
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if failAll {
		return context.DeadlineExceeded
	}
	p.messages = append(p.messages, memMsg{queue: queue, data: data})
	return nil
}
func (p *memProducer) Close() error { return nil }
func (p *memProducer) msgs() []memMsg {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]memMsg, len(p.messages))
	copy(cp, p.messages)
	return cp
}

func TestToMessage(t *testing.T) {
	hookReason := "keyword match"
	event := AuditEvent{
		ID:                    "evt-1",
		TransactionID:         "txn-1",
		ConnectionID:          "conn-1",
		TrafficSource:         "COMPLIANCE_PROXY",
		IngressType:           "CONNECT",
		BumpStatus:            "BUMP_SUCCESS",
		SourceIP:              "10.0.0.1",
		TargetHost:            "api.openai.com",
		Method:                "POST",
		Path:                  "/v1/chat/completions",
		StatusCode:            intPtr(200),
		RequestHookDecision:   "ALLOW",
		RequestHookReason:     &hookReason,
		LatencyMs:             42,
		Timestamp:             time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC),
		TraceID:               "trace-abc",
		Provider:              "openai",
		Model:                 "gpt-4o-mini",
		PromptTokens:          100,
		CompletionTokens:      50,
		TotalTokens:           150,
		APIKeyClass:           "sk-",
		APIKeyFingerprint:     "abcdef0123456789",
		UsageExtractionStatus: "ok",
	}

	msg := toMessage(event, "", "")

	if msg.Source != "compliance-proxy" {
		t.Errorf("Source = %q, want %q", msg.Source, "compliance-proxy")
	}
	if msg.ID != "evt-1" {
		t.Errorf("ID = %q, want %q", msg.ID, "evt-1")
	}
	if msg.TraceID != "trace-abc" {
		t.Errorf("TraceID = %q, want %q", msg.TraceID, "trace-abc")
	}
	if msg.BumpStatus != "BUMP_SUCCESS" {
		t.Errorf("BumpStatus = %q, want %q", msg.BumpStatus, "BUMP_SUCCESS")
	}
	if msg.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", msg.StatusCode)
	}
	if msg.ProviderID != "openai" || msg.ProviderName != "openai" {
		t.Errorf("Provider = (%q,%q), want openai", msg.ProviderID, msg.ProviderName)
	}
	if msg.ModelID != "gpt-4o-mini" || msg.ModelName != "gpt-4o-mini" {
		t.Errorf("Model = (%q,%q), want gpt-4o-mini", msg.ModelID, msg.ModelName)
	}
	if msg.PromptTokens != 100 || msg.CompletionTokens != 50 || msg.TotalTokens != 150 {
		t.Errorf("tokens = (%d,%d,%d), want (100,50,150)",
			msg.PromptTokens, msg.CompletionTokens, msg.TotalTokens)
	}
	if msg.APIKeyClass != "sk-" {
		t.Errorf("APIKeyClass = %q, want sk-", msg.APIKeyClass)
	}
	if msg.APIKeyFingerprint != "abcdef0123456789" {
		t.Errorf("APIKeyFingerprint = %q", msg.APIKeyFingerprint)
	}
	if msg.UsageExtractionStatus != "ok" {
		t.Errorf("UsageExtractionStatus = %q, want ok", msg.UsageExtractionStatus)
	}

	// Details should carry compliance-specific metadata.
	details, ok := msg.Details.(map[string]any)
	if !ok {
		t.Fatalf("Details type = %T, want map", msg.Details)
	}
	if details["transactionId"] != "txn-1" {
		t.Errorf("details.transactionId = %v, want %q", details["transactionId"], "txn-1")
	}
}

// TestToMessage_ThingIdentity verifies that the proxy's Thing ID / name
// passed at emit time lands on TrafficEventMessage so the Hub db-writer
// scans them onto traffic_event.thing_id / thing_name.
func TestToMessage_ThingIdentity(t *testing.T) {
	msg := toMessage(AuditEvent{ID: "evt-thing", Timestamp: time.Now()}, "proxy-host", "host")
	if msg.ThingID != "proxy-host" {
		t.Errorf("ThingID = %q, want proxy-host", msg.ThingID)
	}
	if msg.ThingName != "host" {
		t.Errorf("ThingName = %q, want host", msg.ThingName)
	}
	// Empty pass-through keeps SQL NULL signalling for legacy callers.
	msgEmpty := toMessage(AuditEvent{ID: "evt-empty", Timestamp: time.Now()}, "", "")
	if msgEmpty.ThingID != "" || msgEmpty.ThingName != "" {
		t.Errorf("empty Thing identity should stay empty; got (%q,%q)", msgEmpty.ThingID, msgEmpty.ThingName)
	}
}

// TestToMessage_StampsIdentityPending verifies every CP audit event
// reaches the Hub with identity.status="pending" so the Hub
// IdentityEnricher job can resolve the user via DeviceAssignment
// ip_address lookup. Pre-fix CP left msg.Identity nil → Hub wrote SQL
// NULL → job's `WHERE identity->>'status' = 'pending'` filter skipped
// the row, leaving 3K+ prod rows un-enriched indefinitely.
func TestToMessage_StampsIdentityPending(t *testing.T) {
	msg := toMessage(AuditEvent{ID: "evt-id", Timestamp: time.Now()}, "", "")
	if msg.Identity == nil {
		t.Fatal("Identity is nil; producer must stamp {status:pending}")
	}
	identity, ok := msg.Identity.(map[string]any)
	if !ok {
		t.Fatalf("Identity is %T, want map[string]any from the compliance producer", msg.Identity)
	}
	if identity["status"] != "pending" {
		t.Errorf("Identity.status = %v, want \"pending\"", identity["status"])
	}
}

// TestToMessage_BlockingRule verifies that when a compliance-proxy audit
// event carries a pre-serialised rule-pack BlockingRule payload, the wire
// message exposes it through TrafficEventMessage.RequestBlockingRule so the Hub
// db-writer can persist it onto `traffic_event.blocking_rule`.
func TestToMessage_BlockingRule(t *testing.T) {
	payload := []byte(`{"pack":"content-safety","pack_version":"1.0.0","rule_id":"violence-kill"}`)
	event := AuditEvent{
		ID:                  "evt-br",
		TransactionID:       "txn-br",
		TrafficSource:       "COMPLIANCE_PROXY",
		IngressType:         "CONNECT",
		BumpStatus:          "BUMP_SUCCESS",
		RequestHookDecision: "REJECT_HARD",
		Timestamp:           time.Now(),
		RequestBlockingRule: payload,
	}

	msg := toMessage(event, "", "")

	if msg.RequestBlockingRule == nil {
		t.Fatal("BlockingRule should be set on the wire message")
	}
	var decoded struct {
		Pack        string `json:"pack"`
		PackVersion string `json:"pack_version"`
		RuleID      string `json:"rule_id"`
	}
	if err := json.Unmarshal(*msg.RequestBlockingRule, &decoded); err != nil {
		t.Fatalf("unmarshal RequestBlockingRule: %v", err)
	}
	if decoded.Pack != "content-safety" || decoded.PackVersion != "1.0.0" || decoded.RuleID != "violence-kill" {
		t.Errorf("BlockingRule payload = %+v, want (content-safety, 1.0.0, violence-kill)", decoded)
	}

	empty := toMessage(AuditEvent{ID: "evt-empty", Timestamp: time.Now()}, "", "")
	if empty.RequestBlockingRule != nil {
		t.Errorf("BlockingRule should be nil when not set; got %s", string(*empty.RequestBlockingRule))
	}
}

func TestMQBatchWriter_FlushBatch(t *testing.T) {
	prod := &memProducer{}
	logger := slog.Default()
	w := NewMQBatchWriter(prod, "nexus.event.compliance", 100, 5*time.Second, 100, nil, logger)
	defer w.Close(context.Background()) //nolint:errcheck

	for i := range 5 {
		w.Enqueue(AuditEvent{
			ID:                  "evt-" + string(rune('A'+i)),
			BumpStatus:          "BUMP_SUCCESS",
			RequestHookDecision: "ALLOW",
			Timestamp:           time.Now(),
		})
	}

	if err := w.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	msgs := prod.msgs()
	if len(msgs) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(msgs))
	}

	for _, m := range msgs {
		if m.queue != "nexus.event.compliance" {
			t.Errorf("queue = %q, want %q", m.queue, "nexus.event.compliance")
		}
		var msg mq.TrafficEventMessage
		if err := json.Unmarshal(m.data, &msg); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if msg.Source != "compliance-proxy" {
			t.Errorf("Source = %q, want %q", msg.Source, "compliance-proxy")
		}
		if msg.ID == "" {
			t.Errorf("ID empty — field name mismatch on wire format?")
		}
	}
}

func TestMQBatchWriter_NDJSONFallback(t *testing.T) {
	prod := &memProducer{failAll: true}
	logger := slog.Default()

	w := NewMQBatchWriter(prod, "nexus.event.compliance", 100, 5*time.Second, 100, nil, logger)
	defer w.Close(context.Background()) //nolint:errcheck

	w.Enqueue(AuditEvent{
		ID:                  "evt-fail",
		BumpStatus:          "BUMP_SUCCESS",
		RequestHookDecision: "ALLOW",
		Timestamp:           time.Now(),
	})

	err := w.Flush(context.Background())
	if err == nil {
		t.Error("expected error from MQ failure")
	}

	// No NDJSON writer configured so event is logged as error (no panic).
}

func TestMQBatchWriter_QueueInspector(t *testing.T) {
	prod := &memProducer{}
	logger := slog.Default()
	w := NewMQBatchWriter(prod, "nexus.event.compliance", 100, 5*time.Second, 500, nil, logger)
	defer w.Close(context.Background()) //nolint:errcheck

	if w.QueueLen() != 0 {
		t.Errorf("QueueLen = %d, want 0", w.QueueLen())
	}
	if w.QueueCap() != 500 {
		t.Errorf("QueueCap = %d, want 500", w.QueueCap())
	}

	// Verify it satisfies QueueInspector.
	var _ QueueInspector = w
}

// TestMQBatchWriter_FlushAfterClose covers the Flush <-w.done fallback:
// once Close has shut the loop down, Flush must still drain whatever
// the channel happens to hold without deadlocking on flushReqs (nobody
// is reading from it). The two sub-cases exercise both the empty-drain
// and the non-empty-drain branches of the fallback.
func TestMQBatchWriter_FlushAfterClose(t *testing.T) {
	t.Run("empty channel", func(t *testing.T) {
		prod := &memProducer{}
		w := NewMQBatchWriter(prod, "nexus.event.compliance", 100, 5*time.Second, 100, nil, slog.Default())
		if err := w.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- w.Flush(context.Background()) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Flush after Close: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Flush after Close hung — fallback drain path is wrong")
		}
	})

	t.Run("leftover events drained via fallback", func(t *testing.T) {
		prod := &memProducer{}
		w := NewMQBatchWriter(prod, "nexus.event.compliance", 100, 5*time.Second, 100, nil, slog.Default())
		if err := w.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		// Channel is still open after Close — Enqueue succeeds but no loop
		// is around to drain it. Flush's <-w.done branch must do that work.
		w.Enqueue(AuditEvent{ID: "post-close", BumpStatus: "BUMP_SUCCESS", Timestamp: time.Now()})
		if err := w.Flush(context.Background()); err != nil {
			t.Fatalf("Flush after Close: %v", err)
		}
		if got := len(prod.msgs()); got != 1 {
			t.Fatalf("expected 1 message (post-Close drain), got %d", got)
		}
	})
}

// TestMQBatchWriter_FlushAckTimeout covers Flush's second-select ctx.Done
// branch: flushReqs send succeeds, the loop wedges inside flushBatch (the
// producer blocks), and the caller's deadline fires before the ack arrives.
func TestMQBatchWriter_FlushAckTimeout(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	prod := &memProducer{blockUntil: release, entered: entered}
	w := NewMQBatchWriter(prod, "nexus.event.compliance", 100, 5*time.Second, 100, nil, slog.Default())
	var releaseOnce sync.Once
	releaseProducer := func() { releaseOnce.Do(func() { close(release) }) }
	defer func() {
		releaseProducer()
		_ = w.Close(context.Background())
	}()

	w.Enqueue(AuditEvent{ID: "evt", BumpStatus: "BUMP_SUCCESS", Timestamp: time.Now()})

	// Tiny pause so the loop's main select can read the event from w.ch
	// and return to the select. After this, flushReqs is readable and the
	// next Flush call's first-select succeeds — pushing the timeout into
	// the second select where the ack should arrive (but won't).
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := w.Flush(ctx); err == nil {
		t.Error("Flush with expired ack-wait deadline returned nil; expected ctx.Err()")
	}

	// Sync on producer entered before teardown so cleanup doesn't race.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("producer never reached Enqueue")
	}
}

// TestMQBatchWriter_FlushCtxCancellation covers both Flush ctx.Done()
// branches by wedging the loop inside flushBatch and then issuing two
// follow-up Flush calls — one with an already-cancelled ctx (first
// select wins via ctx.Done) and one with a short-deadline ctx (second
// select wins while waiting for ack).
func TestMQBatchWriter_FlushCtxCancellation(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	prod := &memProducer{blockUntil: release, entered: entered}
	logger := slog.Default()
	w := NewMQBatchWriter(prod, "nexus.event.compliance", 100, 5*time.Second, 100, nil, logger)
	var releaseOnce sync.Once
	releaseProducer := func() { releaseOnce.Do(func() { close(release) }) }
	defer func() {
		releaseProducer()
		_ = w.Close(context.Background())
	}()

	w.Enqueue(AuditEvent{ID: "evt-block", BumpStatus: "BUMP_SUCCESS", Timestamp: time.Now()})

	// First Flush in a goroutine — it'll wedge the loop inside flushBatch.
	firstAck := make(chan error, 1)
	go func() { firstAck <- w.Flush(context.Background()) }()

	// Wait for the producer to actually be inside Enqueue (loop wedged).
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("producer was never entered — loop did not pick up flush request")
	}

	// Branch A: already-cancelled ctx hits the first-select ctx.Done case
	// because flushReqs can't be sent into a busy loop.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Flush(cancelledCtx); err == nil {
		t.Error("Flush with cancelled ctx returned nil; expected ctx.Err()")
	}

	// Branch B: a short deadline expires while we're waiting for the
	// (still-wedged) loop to ack — exercises the second-select ctx.Done.
	deadlineCtx, deadlineCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer deadlineCancel()
	if err := w.Flush(deadlineCtx); err == nil {
		t.Error("Flush with expired deadline returned nil; expected ctx.Err()")
	}

	// Release the producer so the first Flush can complete and Close can drain.
	releaseProducer()
	select {
	case <-firstAck:
	case <-time.After(2 * time.Second):
		t.Fatal("first Flush never returned after release")
	}
}

func intPtr(v int) *int { return &v }
