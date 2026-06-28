package shell

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// fakeRunner drives the bridge without a model. run executes the turn script
// (which may stream events / hit the confirm path) then returns final/err. A nil
// run is a no-op turn returning ("", nil).
type fakeRunner struct {
	run     func(ctx context.Context) (string, error)
	compact func(ctx context.Context) (agent.CompactStat, bool, error)
}

func (f *fakeRunner) Turn(ctx context.Context, _, _ string) (string, error) {
	if f.run == nil {
		return "", nil
	}
	return f.run(ctx)
}

func (f *fakeRunner) Compact(ctx context.Context) (agent.CompactStat, bool, error) {
	if f.compact == nil {
		return agent.CompactStat{}, false, nil
	}
	return f.compact(ctx)
}

// stubTool is a minimal agent.Tool used to name a confirm request.
type stubTool struct{ name string }

func (s stubTool) Name() string            { return s.name }
func (s stubTool) Description() string     { return "" }
func (s stubTool) Schema() json.RawMessage { return nil }
func (s stubTool) Tier() agent.Tier        { return agent.TierConfirm }
func (s stubTool) Run(context.Context, json.RawMessage) (agent.Result, error) {
	return agent.Result{}, nil
}

func TestBridgeNavigateEnqueuesNavMsg(t *testing.T) {
	b := newBridge(&fakeRunner{})
	if err := b.Navigate("cost", core.TrafficFilter{StatusRange: "5xx"}); err != nil {
		t.Fatalf("Navigate err: %v", err)
	}
	msg := <-b.evCh
	nav, ok := msg.(agentNavMsg)
	if !ok {
		t.Fatalf("want agentNavMsg, got %T", msg)
	}
	if nav.view != "cost" || nav.filter.StatusRange != "5xx" {
		t.Fatalf("bad nav msg: %+v", nav)
	}
}

func TestBridgeShowAndHighlightEnqueue(t *testing.T) {
	b := newBridge(&fakeRunner{})
	if err := b.ShowEvent("ev-9a3f"); err != nil {
		t.Fatalf("ShowEvent err: %v", err)
	}
	if m, ok := (<-b.evCh).(agentShowMsg); !ok || m.id != "ev-9a3f" {
		t.Fatalf("want agentShowMsg ev-9a3f, got %#v", m)
	}
	if err := b.Highlight("openai"); err != nil {
		t.Fatalf("Highlight err: %v", err)
	}
	if m, ok := (<-b.evCh).(agentHighlightMsg); !ok || m.ref != "openai" {
		t.Fatalf("want agentHighlightMsg openai, got %#v", m)
	}
}

func TestBridgeConfirmHandshakeApprove(t *testing.T) {
	b := newBridge(&fakeRunner{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cf := b.confirmFunc(ctx)
	got := make(chan bool, 1)
	go func() {
		ok, _ := cf(context.Background(), stubTool{name: "mitigate_cache_flush"}, nil, "mitigation requires authorization")
		got <- ok
	}()

	msg := <-b.evCh
	c, ok := msg.(agentConfirmMsg)
	if !ok || c.tool != "mitigate_cache_flush" {
		t.Fatalf("want agentConfirmMsg for cache flush, got %#v", msg)
	}
	b.replyCh <- true
	if !<-got {
		t.Fatal("approved confirm should return true")
	}
}

func TestBridgeConfirmHandshakeDecline(t *testing.T) {
	b := newBridge(&fakeRunner{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cf := b.confirmFunc(ctx)
	got := make(chan bool, 1)
	go func() { ok, _ := cf(context.Background(), stubTool{name: "mitigate_vk_revoke"}, nil, "r"); got <- ok }()
	<-b.evCh // drain the request
	b.replyCh <- false
	if <-got {
		t.Fatal("declined confirm should return false")
	}
}

func TestBridgeConfirmCtxCancelDeclines(t *testing.T) {
	b := newBridge(&fakeRunner{})
	ctx, cancel := context.WithCancel(context.Background())
	cf := b.confirmFunc(ctx)
	cancel() // turn torn down before the request is read
	if ok, _ := cf(context.Background(), stubTool{name: "x"}, nil, "r"); ok {
		t.Fatal("a cancelled turn must decline (fail-safe)")
	}
}

func TestBridgeConfirmCancelWhileWaitingDeclines(t *testing.T) {
	b := newBridge(&fakeRunner{})
	ctx, cancel := context.WithCancel(context.Background())
	cf := b.confirmFunc(ctx)
	got := make(chan bool, 1)
	go func() { ok, _ := cf(context.Background(), stubTool{name: "m"}, nil, "r"); got <- ok }()
	<-b.evCh // request sent; confirmFunc is now parked on replyCh
	cancel() // cancel while it waits for a reply
	if <-got {
		t.Fatal("cancel while awaiting reply must decline (fail-safe)")
	}
}

func TestBridgeConfirmMethodDeclinesWithNoTurn(t *testing.T) {
	b := newBridge(&fakeRunner{})
	// confirm is the agent.ConfirmFunc wired into the built agent. Called with no
	// turn in flight (turnCtx nil) it must fail-safe decline rather than block.
	if ok, _ := b.confirm(context.Background(), stubTool{name: "mitigate_cache_flush"}, nil, "r"); ok {
		t.Fatal("confirm with no active turn must decline (fail-safe)")
	}
}

func TestBridgeConfirmMethodBindsToTurnCtx(t *testing.T) {
	b := newBridge(&fakeRunner{})
	// Simulate an in-flight turn by setting the turn context the way startTurn does.
	ctx, cancel := context.WithCancel(context.Background())
	b.turnCtx = ctx
	got := make(chan bool, 1)
	go func() {
		ok, _ := b.confirm(context.Background(), stubTool{name: "mitigate_provider_enabled"}, nil, "authorize")
		got <- ok
	}()

	if m, ok := (<-b.evCh).(agentConfirmMsg); !ok || m.tool != "mitigate_provider_enabled" {
		t.Fatalf("want agentConfirmMsg for provider toggle, got %#v", m)
	}
	cancel() // tearing the turn down while the confirm is parked must decline
	if <-got {
		t.Fatal("confirm bound to the turn ctx must decline when that ctx cancels")
	}
}

func TestBridgeStopCancelsTurn(t *testing.T) {
	started := make(chan struct{})
	b := newBridge(&fakeRunner{run: func(ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done() // block until stop() cancels the turn
		return "", ctx.Err()
	}})
	cmd := b.startTurn("hi", "v")
	<-started
	b.stop()
	done, ok := cmd().(agentDoneMsg)
	if !ok || done.err == nil {
		t.Fatalf("stop should cancel the turn → done with err, got %#v", done)
	}
}

func TestBridgeStopNilSafe(t *testing.T) {
	var b *bridge
	b.stop()                        // nil receiver must not panic
	newBridge(&fakeRunner{}).stop() // cancel is nil before any turn started
}

func TestBridgeStartTurnStreamsThenDone(t *testing.T) {
	var b *bridge
	b = newBridge(&fakeRunner{run: func(ctx context.Context) (string, error) {
		b.sendEv(agentTextMsg{delta: "hello"})
		return "hello world", nil
	}})
	cmd := b.startTurn("hi", "overview")
	if cmd == nil {
		t.Fatal("startTurn should return a drain cmd")
	}
	if d, ok := cmd().(agentTextMsg); !ok || d.delta != "hello" {
		t.Fatalf("want text delta hello, got %#v", cmd())
	}
	done, ok := b.drain()().(agentDoneMsg)
	if !ok || done.final != "hello world" || done.err != nil {
		t.Fatalf("want done hello world, got %#v", done)
	}
}

func TestBridgeStartTurnRejectsDoubleStart(t *testing.T) {
	b := newBridge(&fakeRunner{})
	if b.startTurn("a", "v") == nil {
		t.Fatal("first startTurn should return a drain cmd")
	}
	if b.startTurn("b", "v") != nil {
		t.Fatal("a second concurrent turn must be rejected")
	}
}

func TestBridgeTurnErrorRidesOnDone(t *testing.T) {
	b := newBridge(&fakeRunner{run: func(ctx context.Context) (string, error) {
		return "partial", errors.New("step cap reached")
	}})
	cmd := b.startTurn("hi", "overview")
	done, ok := cmd().(agentDoneMsg)
	if !ok || done.final != "partial" || done.err == nil {
		t.Fatalf("want done with partial+err, got %#v", done)
	}
}

func TestBridgeReplyForwardsToLiveTurn(t *testing.T) {
	b := newBridge(&fakeRunner{})
	ctx, cancel := context.WithCancel(context.Background()) // a live (uncancelled) turn
	defer cancel()
	b.turnCtx = ctx
	got := make(chan bool, 1)
	go func() { got <- <-b.replyCh }()
	if cmd := b.reply(true); cmd == nil {
		t.Fatal("reply to a live turn should forward and return a drain cmd")
	}
	if !<-got {
		t.Fatal("reply(true) should forward true on replyCh")
	}
}

// TestBridgeReplyToDeadTurnDropped is the regression guard for the fail-open bug:
// a reply that arrives after the turn ended (or with no turn) must be dropped, not
// parked on the unread channel (which would both leak and leave an orphan value a
// later turn's confirm would wrongly consume — an unauthorized auto-approve). The
// dropped reply still re-issues the drain: the dying turn's buffered done message
// has no other deliverer once a prompt held the loop at zero outstanding drains.
func TestBridgeReplyToDeadTurnDropped(t *testing.T) {
	b := newBridge(&fakeRunner{})
	if b.reply(true) != nil {
		t.Fatal("reply with no turn ever started should be a no-op")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the turn already ended
	b.turnCtx = ctx
	b.evCh <- agentDoneMsg{} // the dying turn's buffered terminal message
	done := make(chan tea.Cmd, 1)
	go func() { done <- b.reply(true) }()
	select {
	case cmd := <-done:
		if cmd == nil {
			t.Fatal("the dead-turn reply must re-issue the drain (the buffered done has no other deliverer)")
		}
		if _, isDone := cmd().(agentDoneMsg); !isDone {
			t.Fatal("the re-issued drain must deliver the buffered done so finish() runs")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reply to a cancelled turn must not block on the unread reply channel")
	}
}

func TestBridgeDoneClearsRunning(t *testing.T) {
	b := newBridge(&fakeRunner{})
	_ = b.startTurn("a", "v")
	if !b.running {
		t.Fatal("startTurn should mark running")
	}
	b.done()
	if b.running {
		t.Fatal("done should clear running")
	}
}
