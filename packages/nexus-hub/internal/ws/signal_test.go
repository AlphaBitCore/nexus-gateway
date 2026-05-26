package ws

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// fakeMQConsumer is the smallest mq.Consumer implementation that satisfies
// SubscribeHubSignals. It captures the handler so the test can inject
// messages, then optionally blocks/returns to drive the post-Subscribe
// error branch (context.Canceled vs hard error).
type fakeMQConsumer struct {
	handler      mq.MessageHandler
	subscribeErr error
	// returnAfter blocks Subscribe until ctx is done; if false, Subscribe
	// returns immediately with subscribeErr.
	returnAfter bool
	subscribed  chan struct{}
}

func newFakeMQ(returnAfter bool, subscribeErr error) *fakeMQConsumer {
	return &fakeMQConsumer{
		subscribeErr: subscribeErr,
		returnAfter:  returnAfter,
		subscribed:   make(chan struct{}, 1),
	}
}

func (f *fakeMQConsumer) Subscribe(ctx context.Context, _ string, h mq.MessageHandler) error {
	f.handler = h
	select {
	case f.subscribed <- struct{}{}:
	default:
	}
	if f.returnAfter {
		<-ctx.Done()
	}
	return f.subscribeErr
}

func (f *fakeMQConsumer) Consume(_ context.Context, _, _ string, _ mq.MessageHandler) error {
	return nil
}

func (f *fakeMQConsumer) Close() error { return nil }

// invoke synchronously delivers a payload to the captured handler.
func (f *fakeMQConsumer) invoke(t *testing.T, data []byte) {
	t.Helper()
	if f.handler == nil {
		t.Fatal("handler not yet captured; await subscribed first")
	}
	if err := f.handler(context.Background(), &mq.Message{Data: data}); err != nil {
		t.Fatalf("handler returned err: %v", err)
	}
}

func waitSubscribed(t *testing.T, fmq *fakeMQConsumer) {
	t.Helper()
	select {
	case <-fmq.subscribed:
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe was never invoked")
	}
}

// installPoolWithRecorder substitutes ws.Pool.Send/Broadcast capture by
// using a real pool with a stub Conn whose outCh records writes.
type recorder struct {
	mu       sync.Mutex
	sends    [][]byte
	bcasts   [][]byte
	bcastIDs []string
}

func (r *recorder) addSend(d []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]byte, len(d))
	copy(cp, d)
	r.sends = append(r.sends, cp)
}

func (r *recorder) addBcast(id string, d []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]byte, len(d))
	copy(cp, d)
	r.bcasts = append(r.bcasts, cp)
	r.bcastIDs = append(r.bcastIDs, id)
}

func (r *recorder) sendCount() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.sends) }

// buildPoolWithRecord creates a Pool with one Thing-typed Conn registered
// per (id, type) tuple. Writes are funnelled into the recorder.
func buildPoolWithRecord(t *testing.T, recs *recorder, things []struct{ ID, Type string }) *Pool {
	t.Helper()
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	p := NewPool(reg, nullLogger())
	for _, th := range things {
		c := &Conn{
			thingID:   th.ID,
			thingType: th.Type,
			outCh:     make(chan []byte, 8),
			done:      make(chan struct{}),
			logger:    nullLogger(),
		}
		p.Add(c)
		// Drain outCh into the recorder so Send/Broadcast capture the write.
		go func(c *Conn, thID string) {
			for {
				select {
				case d := <-c.outCh:
					recs.addSend(d)
					recs.addBcast(thID, d)
				case <-c.done:
					return
				}
			}
		}(c, th.ID)
	}
	return p
}

// TestSubscribeHubSignals_BadJSON exercises the json.Unmarshal-failure
// branch of the handler — handler returns nil, no Send/Broadcast.
func TestSubscribeHubSignals_BadJSON(t *testing.T) {
	recs := &recorder{}
	pool := buildPoolWithRecord(t, recs, []struct{ ID, Type string }{{"agent-1", "agent"}})
	fmq := newFakeMQ(true, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go SubscribeHubSignals(ctx, fmq, pool, "this-hub", nullLogger())
	waitSubscribed(t, fmq)

	fmq.invoke(t, []byte("not-json"))
	if recs.sendCount() != 0 {
		t.Fatalf("bad-JSON signal must not Send/Broadcast, got %d", recs.sendCount())
	}
}

// TestSubscribeHubSignals_SameHubSkipped ensures signals tagged with the
// receiving hub's id are not re-broadcast (loop-prevention).
func TestSubscribeHubSignals_SameHubSkipped(t *testing.T) {
	recs := &recorder{}
	pool := buildPoolWithRecord(t, recs, []struct{ ID, Type string }{{"agent-1", "agent"}})
	fmq := newFakeMQ(true, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go SubscribeHubSignals(ctx, fmq, pool, "this-hub", nullLogger())
	waitSubscribed(t, fmq)

	payload := `{"action":"config_changed","sourceHub":"this-hub","thingId":"agent-1"}`
	fmq.invoke(t, []byte(payload))
	if recs.sendCount() != 0 {
		t.Fatalf("same-hub signal must be skipped, got %d sends", recs.sendCount())
	}
}

// TestSubscribeHubSignals_ConfigChangedToThingID sends a config_changed
// signal addressed by ThingID — verifies Pool.Send is invoked.
func TestSubscribeHubSignals_ConfigChangedToThingID(t *testing.T) {
	recs := &recorder{}
	pool := buildPoolWithRecord(t, recs, []struct{ ID, Type string }{{"agent-1", "agent"}})
	fmq := newFakeMQ(true, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go SubscribeHubSignals(ctx, fmq, pool, "this-hub", nullLogger())
	waitSubscribed(t, fmq)

	payload := `{"action":"config_changed","sourceHub":"other-hub","thingId":"agent-1","configKey":"hooks","version":3,"force":true}`
	fmq.invoke(t, []byte(payload))

	// Allow drainer goroutine to capture the write.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if recs.sendCount() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if recs.sendCount() != 1 {
		t.Fatalf("expected 1 Send to agent-1, got %d", recs.sendCount())
	}
}

// TestSubscribeHubSignals_ConfigChangedToThingType broadcasts when no
// ThingID is specified but a ThingType is.
func TestSubscribeHubSignals_ConfigChangedToThingType(t *testing.T) {
	recs := &recorder{}
	pool := buildPoolWithRecord(t, recs, []struct{ ID, Type string }{
		{"agent-1", "agent"},
		{"agent-2", "agent"},
		{"cp-1", "control-plane"},
	})
	fmq := newFakeMQ(true, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go SubscribeHubSignals(ctx, fmq, pool, "this-hub", nullLogger())
	waitSubscribed(t, fmq)

	payload := `{"action":"config_changed","sourceHub":"other-hub","thingType":"agent","configKey":"hooks","version":1}`
	fmq.invoke(t, []byte(payload))

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if recs.sendCount() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if recs.sendCount() != 2 {
		t.Fatalf("expected 2 Broadcast to agents, got %d", recs.sendCount())
	}
}

// TestSubscribeHubSignals_ConfigChangedNoTarget covers the no-thingId-no-thingType
// branch — the message is parsed, both routing arms are skipped, no panic.
func TestSubscribeHubSignals_ConfigChangedNoTarget(t *testing.T) {
	recs := &recorder{}
	pool := buildPoolWithRecord(t, recs, nil)
	fmq := newFakeMQ(true, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go SubscribeHubSignals(ctx, fmq, pool, "this-hub", nullLogger())
	waitSubscribed(t, fmq)

	fmq.invoke(t, []byte(`{"action":"config_changed","sourceHub":"other-hub"}`))
	if recs.sendCount() != 0 {
		t.Fatalf("no-target signal must not fan out, got %d", recs.sendCount())
	}
}

// TestSubscribeHubSignals_UnknownAction covers the default switch arm.
func TestSubscribeHubSignals_UnknownAction(t *testing.T) {
	recs := &recorder{}
	pool := buildPoolWithRecord(t, recs, []struct{ ID, Type string }{{"agent-1", "agent"}})
	fmq := newFakeMQ(true, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go SubscribeHubSignals(ctx, fmq, pool, "this-hub", nullLogger())
	waitSubscribed(t, fmq)

	fmq.invoke(t, []byte(`{"action":"some_unknown","sourceHub":"other-hub"}`))
	if recs.sendCount() != 0 {
		t.Fatalf("unknown action must not Send, got %d", recs.sendCount())
	}
}

// TestSubscribeHubSignals_CtxCancelExit verifies that a Subscribe that
// returns context.Canceled is logged as "subscription ended" and the
// function returns cleanly.
func TestSubscribeHubSignals_CtxCancelExit(t *testing.T) {
	fmq := newFakeMQ(true, context.Canceled)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		SubscribeHubSignals(ctx, fmq, NewPool(nil, nullLogger()), "this-hub", nullLogger())
		close(done)
	}()
	waitSubscribed(t, fmq)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SubscribeHubSignals did not return after ctx cancel")
	}
}

// TestSubscribeHubSignals_HardErrorExit verifies a non-cancel error
// terminates the function via the error log branch.
func TestSubscribeHubSignals_HardErrorExit(t *testing.T) {
	fmq := newFakeMQ(false, errors.New("connection lost"))
	done := make(chan struct{})
	go func() {
		SubscribeHubSignals(context.Background(), fmq, NewPool(nil, nullLogger()), "this-hub", nullLogger())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SubscribeHubSignals did not return on hard error")
	}
}

// TestSubscribeHubSignals_ConfigChangedMarshalsExpectedShape ensures the
// broadcast payload includes Type=config_changed and propagates Force.
func TestSubscribeHubSignals_ConfigChangedMarshalsExpectedShape(t *testing.T) {
	recs := &recorder{}
	pool := buildPoolWithRecord(t, recs, []struct{ ID, Type string }{{"agent-1", "agent"}})
	fmq := newFakeMQ(true, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go SubscribeHubSignals(ctx, fmq, pool, "this-hub", nullLogger())
	waitSubscribed(t, fmq)

	payload := `{"action":"config_changed","sourceHub":"other-hub","thingId":"agent-1","configKey":"k","state":{"x":1},"version":9,"force":true,"desired":{"a":"b"}}`
	fmq.invoke(t, []byte(payload))

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if recs.sendCount() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if recs.sendCount() != 1 {
		t.Fatalf("expected 1 Send, got %d", recs.sendCount())
	}
	recs.mu.Lock()
	got := string(recs.sends[0])
	recs.mu.Unlock()
	for _, want := range []string{
		`"type":"config_changed"`,
		`"configKey":"k"`,
		`"desiredVer":9`,
		`"force":true`,
	} {
		if !contains(got, want) {
			t.Errorf("broadcast missing %q\nfull: %s", want, got)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// Compile-time guard: fakeMQConsumer must satisfy mq.Consumer.
var _ mq.Consumer = (*fakeMQConsumer)(nil)

// silence unused
var _ = fmt.Sprintf
