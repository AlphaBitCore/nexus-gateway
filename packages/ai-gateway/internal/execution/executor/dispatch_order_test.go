package executor

// The dispatch-order invariant: every target passed over before the one that
// serves must have been passed over for a recorded reason.
//
// Not "the first target serves" — failover is normal and must stay silent, or
// the check fires on ordinary traffic and gets turned off. What it detects is a
// target that vanished from the loop's own account of itself, which is the
// state all three ordering defects were found in.

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"

	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

func orderLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	// Debug level: the check emits a positive line when it finds no violation,
	// and that line is the only thing an end-to-end test can assert POSITIVELY.
	// At Warn the suite could not tell a correct loop from one that never runs
	// the check.
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func targetsNamed(ids ...string) []routingcore.RoutingTarget {
	out := make([]routingcore.RoutingTarget, 0, len(ids))
	for _, id := range ids {
		out = append(out, routingcore.RoutingTarget{ModelID: id, ModelCode: id})
	}
	return out
}

// walkNamed builds the per-target state the check now reads, so a test states
// which targets were accounted for rather than passing a bare bool slice
// alongside a target slice and trusting the two to line up.
func walkNamed(ids []string, explained ...bool) []walkState {
	w := newWalk(targetsNamed(ids...), 2)
	for i := range explained {
		w[i].explained = explained[i]
	}
	return w
}

func TestDispatchOrder_WarnsOnAnUnexplainedSkip(t *testing.T) {
	log, buf := orderLogger()
	e := New(nil, nil, nil, nil).WithLogger(log)

	// "first" was passed over silently.
	e.checkDispatchOrder(walkNamed([]string{"first", "second", "third"}, false, true, false), 2)

	out := buf.String()
	if !strings.Contains(out, "routing invariant") {
		t.Fatalf("dispatching position 2 while position 0 was never accounted for "+
			"produced no warning:\n%s", out)
	}
	for _, want := range []string{"third", "first"} {
		if !strings.Contains(out, want) {
			t.Errorf("the warning does not name %q — it has to say which target is "+
				"serving and which one was skipped, or it sends the reader back to "+
				"the database:\n%s", want, out)
		}
	}
	if strings.Contains(out, "second") {
		t.Errorf("the warning names %q, which WAS accounted for; listing explained "+
			"targets makes every real finding harder to read:\n%s", "second", out)
	}
}

// The case that decides whether this can stay on: a plain failover, where every
// earlier target really did fail and was recorded. It must be silent. A check
// that fires here fires on ordinary production traffic.
func TestDispatchOrder_SilentOnAnOrdinaryFailover(t *testing.T) {
	log, buf := orderLogger()
	e := New(nil, nil, nil, nil).WithLogger(log)

	e.checkDispatchOrder(walkNamed([]string{"first", "second"}, true, false), 1)
	if strings.Contains(buf.String(), "routing invariant") {
		t.Errorf("a failover onto the second target after the first was recorded as "+
			"failed must not warn:\n%s", buf.String())
	}
}

func TestDispatchOrder_SilentForThePrimaryAndWithoutALogger(t *testing.T) {
	log, buf := orderLogger()
	e := New(nil, nil, nil, nil).WithLogger(log)
	e.checkDispatchOrder(walkNamed([]string{"first", "second"}, false, false), 0)
	if buf.Len() != 0 {
		t.Errorf("serving position 0 skips nothing and cannot violate an ordering, so "+
			"it must produce nothing at all — not even the verified line, which would "+
			"put one log entry on every single request:\n%s", buf.String())
	}

	// No logger: the check is an observation, and a caller that wires none must
	// not pay for it or panic on it.
	New(nil, nil, nil, nil).checkDispatchOrder(walkNamed([]string{"a", "b"}, false, false), 1)
}

// THE CALL SITE, driven through Execute rather than by calling the checker.
//
// Every assertion above builds the explained[] slice itself, so all of them
// pass against a loop that never calls checkDispatchOrder and against one that
// forgets to mark a skip. This runs the real loop over a list containing the
// one skip that records no Attempt — an armed context-upgrade target passed
// over because nothing overflowed — and requires silence.
//
// It is the negative direction on purpose: the loop is currently correct, so
// the only honest end-to-end assertion is that a correct loop stays quiet. The
// positive direction is proven by mutation — removing the `explained[tIdx] =
// true` beside that skip turns this red, which is recorded in the task notes.
func TestDispatchOrder_ArmedUpgradeSkipIsAccountedForByTheRealLoop(t *testing.T) {
	log, buf := orderLogger()
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 500, Code: provcore.CodeUpstreamError, Message: "boom"}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	exec := New(newRegistry(t, adapter), okResolver(), nil, nil).WithLogger(log)

	upgrade := target(providerSlug)
	upgrade.ContextUpgradeOnly = true
	targets := []routingcore.RoutingTarget{target(providerSlug), upgrade, target(providerSlug)}

	result := exec.Execute(context.Background(), targets, baseReq(), fastBackoffPolicy(1))
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected the third target to serve; status=%d err=%v", result.StatusCode, result.Error)
	}
	if strings.Contains(buf.String(), "routing invariant") {
		t.Errorf("an armed context-upgrade target skipped for the documented reason "+
			"tripped the invariant. It fires on ordinary traffic in that state, which "+
			"is how a check like this gets switched off:\n%s", buf.String())
	}
	// And it RAN. Asserting only the absence of a warning passes against a loop
	// with no check in it at all — proven by mutation, which is why this line
	// is here.
	if !strings.Contains(buf.String(), "dispatch order verified") {
		t.Fatalf("the real Execute loop never ran the dispatch-order check.\n"+
			"  every other test in this file builds explained[] itself and passes "+
			"whether or not the loop calls it.\n  log was:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "position=2") {
		t.Errorf("the verified line does not say which position served; it is also "+
			"the answer to \"how far down the list did this request get\":\n%s", buf.String())
	}
}

// TestDispatchOrder_SilentOnADeliberateJump.
//
// The gap check asks whether every target passed over has a recorded reason,
// and it asks it by position. Selection is no longer positional: a context
// overflow reaches for the largest remaining window and a rate limit steps off
// its provider, both leaving untouched entries behind them on purpose.
//
// Two independent reviews found this firing on ordinary traffic, with the
// logger wired in production. An invariant that warns on healthy requests is
// one nobody reads on the unhealthy ones.
func TestDispatchOrder_SilentOnADeliberateJump(t *testing.T) {
	var logged strings.Builder
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn}))

	small := target("small")
	small.MaxContextTokens = 8_000
	middle := target("middle")
	middle.MaxContextTokens = 16_000
	roomy := target("roomy")
	roomy.MaxContextTokens = 200_000

	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Code: provcore.CodeContextOverflow, Status: 400, Message: "too long"}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`served`)}},
	}}
	exec := New(newRegistry(t, adapter), okResolver(), nil, nil).WithLogger(logger)

	// The overflow on `small` sends the walk to `roomy`, stepping over `middle`.
	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{small, middle, roomy}, baseReq(), fastBackoffPolicy(1))

	if result.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", result.StatusCode)
	}
	if strings.Contains(logged.String(), "routing invariant") {
		t.Errorf("a deliberate largest-window jump was reported as an unexplained gap:\n%s", logged.String())
	}
}
