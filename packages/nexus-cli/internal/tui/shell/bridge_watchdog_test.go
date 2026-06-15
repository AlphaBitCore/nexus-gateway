package shell

import (
	"context"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
)

// TestBridge_IdleWatchdogSeversOnlyStuckTurns pins the watchdog contract that
// replaced the fixed total cap: a turn continuously making progress runs past
// many idle windows untouched, while a turn that goes silent is cancelled —
// the "stuck model never wedges the UI" intent without severing live streams.
func TestBridge_IdleWatchdogSeversOnlyStuckTurns(t *testing.T) {
	// Active turn: streams a delta every 5ms for 40x the idle window; must finish on its own.
	active := newBridge(nil)
	active.idleTimeout = 10 * time.Millisecond
	active.agent = agentFunc(func(ctx context.Context) (string, error) {
		for i := 0; i < 80; i++ {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(5 * time.Millisecond):
				active.sendEv(agentTextMsg{delta: "w"})
			}
		}
		return "done", nil
	})
	runTurn(t, active, func(err error) {
		if err != nil {
			t.Fatalf("an actively streaming turn must never be severed: %v", err)
		}
	})

	// Stuck turn: emits nothing; the watchdog must cancel it.
	stuck := newBridge(nil)
	stuck.idleTimeout = 15 * time.Millisecond
	stuck.agent = agentFunc(func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	runTurn(t, stuck, func(err error) {
		if err == nil {
			t.Fatal("a turn with no progress must be cancelled by the idle watchdog")
		}
	})
}

// TestBridge_IdleWatchdogPausesForConfirm: a confirm card waiting on the human
// pauses the clock — thinking time never kills the turn.
func TestBridge_IdleWatchdogPausesForConfirm(t *testing.T) {
	b := newBridge(nil)
	b.idleTimeout = 20 * time.Millisecond
	b.confirmWait.Add(1) // simulate a raised confirm card
	b.agent = agentFunc(func(ctx context.Context) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(120 * time.Millisecond): // 6x the idle window, zero events
			return "pondered", nil
		}
	})
	runTurn(t, b, func(err error) {
		if err != nil {
			t.Fatalf("the watchdog must pause while a confirm waits on the human: %v", err)
		}
	})
}

// agentFunc adapts a turn closure onto AgentRunner.
type agentFunc func(ctx context.Context) (string, error)

func (f agentFunc) Turn(ctx context.Context, _, _ string) (string, error) { return f(ctx) }
func (f agentFunc) Compact(context.Context) (agent.CompactStat, bool, error) {
	return agent.CompactStat{}, false, nil
}

// runTurn drives one startTurn to its done message and hands the error to check.
func runTurn(t *testing.T, b *bridge, check func(error)) {
	t.Helper()
	if cmd := b.startTurn("q", ""); cmd == nil {
		t.Fatal("startTurn refused")
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-b.evCh:
			if done, ok := m.(agentDoneMsg); ok {
				b.done()
				check(done.err)
				return
			}
		case <-deadline:
			t.Fatal("turn never finished")
		}
	}
}
