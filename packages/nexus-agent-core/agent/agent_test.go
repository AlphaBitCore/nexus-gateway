package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func newTestAgent(t *testing.T, model Model, reg *Registry) *Agent {
	t.Helper()
	dir := t.TempDir()
	mem := OpenMemoryStore(dir, "local")
	store := openStoreAt(dir)
	return New(Config{
		Model:     model,
		Registry:  reg,
		Gate:      NewGate(NewCommandClassifier(), nil, false),
		Memory:    mem,
		Store:     store,
		Compactor: NewCompactor(model, 0),
		Situation: fakeSituation{s: Situation{Health: "5 nodes online"}},
		Env:       "local",
		IsProd:    false,
		Session:   NewSession("local"),
	})
}

func TestAgentTurnAssemblesContextAndPersists(t *testing.T) {
	reg := NewRegistry()
	fm := newFakeModel(asstText("5 nodes are healthy."))
	a := newTestAgent(t, fm, reg)

	if err := a.Memory.Remember(MemoryFact{
		Name: "fleet-latency-baseline", Description: "normal p95 ~ 90ms",
		Type: MemBaseline, Body: "the fleet's normal p95 latency is about 90ms",
	}); err != nil {
		t.Fatal(err)
	}
	reply, err := a.Turn(context.Background(), "is the fleet healthy?", "Mission Control: 5 nodes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "healthy") {
		t.Fatalf("Turn returns the assistant reply, got %q", reply)
	}
	req := fm.gotReqs[0]
	blob := req.System
	for _, m := range req.Messages {
		blob += m.Text()
	}
	for _, must := range []string{"5 nodes online", "normal p95 ~ 90ms", "Mission Control"} {
		if !strings.Contains(blob, must) {
			t.Fatalf("turn must inject %q into the model request:\n%s", must, blob)
		}
	}
	metas, _ := a.Store.List()
	if len(metas) != 1 {
		t.Fatalf("turn must persist the session, got %d", len(metas))
	}
	loaded, _ := a.Store.Load(a.Session.ID)
	if len(loaded.Messages) < 2 {
		t.Fatalf("persisted transcript should hold the turn, got %d msgs", len(loaded.Messages))
	}
}

func TestAgentTurnDoesNotPersistSituationBundle(t *testing.T) {
	reg := NewRegistry()
	fm := newFakeModel(asstText("ok"))
	a := newTestAgent(t, fm, reg)

	if _, err := a.Turn(context.Background(), "is the fleet healthy?", "Mission Control: 5 nodes"); err != nil {
		t.Fatal(err)
	}
	// The model request carries the fresh situation snapshot this turn...
	var sent string
	for _, m := range fm.gotReqs[0].Messages {
		sent += m.Text()
	}
	if !strings.Contains(sent, "5 nodes online") {
		t.Fatal("the model request must carry the fresh situation snapshot")
	}
	// ...but the persisted transcript must NOT, so the system+history prefix stays
	// byte-stable turn-over-turn (prompt-cache stability) and isn't bloated by a
	// repeated frozen snapshot.
	loaded, _ := a.Store.Load(a.Session.ID)
	var stored string
	for _, m := range loaded.Messages {
		stored += m.Text()
	}
	if strings.Contains(stored, "5 nodes online") || strings.Contains(stored, "Live situation") {
		t.Fatalf("persisted history must not carry the situation bundle:\n%s", stored)
	}
	if !strings.Contains(stored, "is the fleet healthy?") {
		t.Fatalf("persisted history must keep the user's actual text, got:\n%s", stored)
	}
}

func TestAgentProdPromptFlag(t *testing.T) {
	reg := NewRegistry()
	fm := newFakeModel(asstText("ok"))
	dir := t.TempDir()
	a := New(Config{
		Model: fm, Registry: reg, Gate: NewGate(nil, nil, false), Memory: OpenMemoryStore(dir, "prod"), Store: openStoreAt(dir),
		Compactor: NewCompactor(fm, 0), Situation: fakeSituation{}, Env: "prod", IsProd: true, Session: NewSession("prod"),
	})
	a.Turn(context.Background(), "hi", "")
	if !strings.Contains(strings.ToUpper(fm.gotReqs[0].System), "PRODUCTION ENVIRONMENT") {
		t.Fatal("prod agent must build a prod-flagged system prompt")
	}
}

func TestAgentRegistersMemoryTools(t *testing.T) {
	reg := NewRegistry()
	fm := newFakeModel(asstText("ok"))
	a := newTestAgent(t, fm, reg)
	for _, n := range []string{"recall", "remember", "update_memory", "forget"} {
		if _, ok := reg.Get(n); !ok {
			t.Fatalf("agent must register the %q builtin", n)
		}
	}
	rt, _ := reg.Get("remember")
	rt.Run(context.Background(), json.RawMessage(`{"type":"entity","title":"primary region","fact":"region us-east-1"}`))
	// the index (loaded each turn) shows the fact; recall returns its body.
	idx, _ := a.Memory.Index()
	if !strings.Contains(idx, "primary region") {
		t.Fatalf("remember builtin must add the fact to the loaded index, got %q", idx)
	}
	f, ok, _ := a.Memory.Recall("primary region")
	if !ok || !strings.Contains(f.Body, "us-east-1") {
		t.Fatalf("recall must return the remembered fact body, got ok=%v body=%q", ok, f.Body)
	}
}

func TestAgentTurnBoundsModelViewButPersistsFullHistory(t *testing.T) {
	reg := NewRegistry()
	fm := newFakeModel(asstText("done"))
	dir := t.TempDir()
	// Tiny trim budget so the seeded prior turns are bounded in the model view.
	comp := NewCompactor(fm, 0)
	comp.trimBudget = 200
	comp.keepRecent = 2
	var trimmed *CompactStat
	a := New(Config{
		Model: fm, Registry: reg, Gate: NewGate(nil, nil, false), Memory: OpenMemoryStore(dir, "local"), Store: openStoreAt(dir),
		Compactor: comp, Situation: fakeSituation{}, Env: "local", Session: NewSession("local"),
		OnCompact: func(s CompactStat) { trimmed = &s },
	})
	// Seed prior alternating turns whose bodies are large enough to exceed the budget.
	for i := 0; i < 8; i++ {
		role := RoleUser
		if i%2 == 1 {
			role = RoleAssistant
		}
		a.Session.Messages = append(a.Session.Messages, TextMessage(role, fmt.Sprintf("old%d %s", i, strings.Repeat("z", 200))))
	}

	reply, err := a.Turn(context.Background(), "new question", "")
	if err != nil || reply != "done" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	// Auto-trim fired (deterministic) and surfaced a visible "trim" stat.
	if trimmed == nil || trimmed.Kind != "trim" {
		t.Fatalf("auto bounding must fire OnCompact with a trim stat, got %v", trimmed)
	}
	// The single model request this turn stayed under the trim budget — no overflow.
	req := fm.gotReqs[0]
	if est := estimateMessages(req.Messages); est > comp.trimBudget {
		t.Fatalf("the model request must be bounded under the trim budget, got %d > %d", est, comp.trimBudget)
	}
	// No summary model call (deterministic path).
	if req.System == compactInstruction {
		t.Fatal("auto bounding must not make a summary model call")
	}
	// The persisted session keeps the FULL transcript — old turns are NOT dropped or elided.
	full := ""
	for _, m := range a.Session.Messages {
		full += m.Text()
	}
	if !strings.Contains(full, "old0") || !strings.Contains(full, "new question") {
		t.Fatalf("persisted history must keep the full transcript (§5.11):\n%s", full)
	}
	if strings.Contains(full, elidedPlaceholder) {
		t.Fatal("the persisted transcript must NOT contain elided placeholders — trimming is model-view-only")
	}
}

func TestAgentManualCompactRewritesAndPersists(t *testing.T) {
	reg := NewRegistry()
	fm := newFakeModel(asstText("COMPACT SUMMARY"))
	dir := t.TempDir()
	comp := NewCompactor(fm, 0) // huge auto budget → only a manual /compact acts
	comp.keepRecent = 2
	comp.keepTarget = 2
	var fired *CompactStat
	a := New(Config{
		Model: fm, Registry: reg, Gate: NewGate(nil, nil, false), Memory: OpenMemoryStore(dir, "local"), Store: openStoreAt(dir),
		Compactor: comp, Situation: fakeSituation{}, Env: "local", Session: NewSession("local"),
		OnCompact: func(s CompactStat) { fired = &s },
	})
	for i := 0; i < 8; i++ {
		role := RoleUser
		if i%2 == 1 {
			role = RoleAssistant
		}
		a.Session.Messages = append(a.Session.Messages, TextMessage(role, fmt.Sprintf("m%d", i)))
	}

	stat, acted, err := a.Compact(context.Background())
	if err != nil || !acted {
		t.Fatalf("manual compact must act, acted=%v err=%v", acted, err)
	}
	if fired == nil {
		t.Fatal("manual compact must fire OnCompact so the UI can surface a notice")
	}
	if stat.MessagesAfter >= stat.MessagesBefore {
		t.Fatalf("manual compact must shrink the transcript, %d→%d", stat.MessagesBefore, stat.MessagesAfter)
	}
	// Unlike auto-compaction, a manual /compact PERMANENTLY rewrites the persisted
	// session: the summary stands in for the older turns on disk too.
	if a.Session.Messages[0].Role != RoleUser || !strings.Contains(a.Session.Messages[0].Text(), "COMPACT SUMMARY") {
		t.Fatalf("manual compact must rewrite the session with the summary first, got %q", a.Session.Messages[0].Text())
	}
	full := ""
	for _, m := range a.Session.Messages {
		full += m.Text()
	}
	if strings.Contains(full, "m0") {
		t.Fatal("manual compact must summarize the older turns away from the persisted session (permanent rewrite)")
	}
	loaded, err := a.Store.Load(a.Session.ID)
	if err != nil || len(loaded.Messages) != len(a.Session.Messages) {
		t.Fatalf("manual compact must persist the rewritten session, loaded=%v err=%v", loaded, err)
	}
}

func TestAgentManualCompactSurfacesModelError(t *testing.T) {
	reg := NewRegistry()
	fm := &fakeModel{errs: []error{errors.New("summary model down")}}
	dir := t.TempDir()
	comp := NewCompactor(fm, 0)
	comp.keepRecent = 2
	comp.keepTarget = 2
	a := New(Config{
		Model: fm, Registry: reg, Gate: NewGate(nil, nil, false), Memory: OpenMemoryStore(dir, "local"), Store: openStoreAt(dir),
		Compactor: comp, Situation: fakeSituation{}, Env: "local", Session: NewSession("local"),
	})
	for i := 0; i < 6; i++ {
		role := RoleUser
		if i%2 == 1 {
			role = RoleAssistant
		}
		a.Session.Messages = append(a.Session.Messages, TextMessage(role, fmt.Sprintf("m%d", i)))
	}
	before := len(a.Session.Messages)

	_, acted, err := a.Compact(context.Background())
	if err == nil || acted {
		t.Fatalf("a summary model error must surface and not claim a compaction, acted=%v err=%v", acted, err)
	}
	if len(a.Session.Messages) != before {
		t.Fatal("a failed manual compact must leave the persisted session unchanged")
	}
}

func TestAgentManualCompactNoOpWhenNothingToCompact(t *testing.T) {
	reg := NewRegistry()
	fm := newFakeModel(asstText("unused"))
	a := newTestAgent(t, fm, reg)
	a.Session.Messages = append(a.Session.Messages, TextMessage(RoleUser, "hi"), TextMessage(RoleAssistant, "yo"))

	_, acted, err := a.Compact(context.Background())
	if err != nil || acted {
		t.Fatalf("nothing to compact must report acted=false with no error, acted=%v err=%v", acted, err)
	}
	if fm.calls != 0 {
		t.Fatalf("a no-op manual compact must not call the model, calls=%d", fm.calls)
	}
}

func TestAgentTurnStepCapReturnsSignalAndPersists(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&stubTool{name: "loop_tool", tier: TierAuto})
	// DefaultStepCap tool_use turns == the agent's cap → the loop never reaches a
	// final answer and Turn surfaces ErrStepCap.
	resps := make([]*ModelResponse, DefaultStepCap)
	for i := range resps {
		resps[i] = asstToolUse("u", "loop_tool", `{}`)
	}
	fm := &fakeModel{resps: resps}
	a := newTestAgent(t, fm, reg)

	reply, err := a.Turn(context.Background(), "go", "")
	if !errors.Is(err, ErrStepCap) {
		t.Fatalf("a runaway turn must surface ErrStepCap, got %v", err)
	}
	// No assistant text was produced (pure tool-use), so finalText falls back to "".
	if reply != "" {
		t.Fatalf("no final answer → empty reply, got %q", reply)
	}
	// The partial turn is still persisted so it is resumable.
	if metas, _ := a.Store.List(); len(metas) != 1 {
		t.Fatalf("a capped turn must still persist, got %d sessions", len(metas))
	}
}

// TestAgentToolNames asserts ToolNames returns the registered tools and the
// kernel memory builtins — and nothing else. This is the bounded set callers
// clamp model-emitted tool names against (an unrecognized name must NOT be a
// real tool, so it collapses to "unknown" at the metric/sink boundary rather
// than becoming an unbounded label).
func TestAgentToolNames(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&stubTool{name: "observe_traffic", tier: TierAuto})
	a := newTestAgent(t, newFakeModel(asstText("ok")), reg)

	set := map[string]bool{}
	for _, n := range a.ToolNames() {
		set[n] = true
	}
	for _, want := range []string{"observe_traffic", "recall", "remember", "update_memory", "forget"} {
		if !set[want] {
			t.Errorf("ToolNames missing %q; got %v", want, a.ToolNames())
		}
	}
	// A name that is not a registered tool nor a builtin must be absent — that is
	// the whole point of the bounded set.
	if set["definitely_not_a_tool"] {
		t.Errorf("ToolNames must not contain a non-tool name")
	}
}

// TestAgentTurnAutoCompactsPastThreshold: a clean turn whose ACTUAL prompt
// usage crosses the window threshold durably compacts the session right away —
// the same summarize-and-rewrite the manual /compact runs, with the OnCompact
// notice — instead of waiting for a human while the trimmer re-elides the same
// transcript every turn. A turn below the threshold leaves the session alone.
func TestAgentTurnAutoCompactsPastThreshold(t *testing.T) {
	reg := NewRegistry()
	// Response 1: the turn's answer, billed OVER threshold (window 1000 → fires
	// at 700). Response 2 is consumed by the auto-compaction summary call.
	heavy := asstText("done with the audit.")
	heavy.Usage = &Usage{PromptTokens: 800, TotalTokens: 820}
	fm := newFakeModel(heavy, asstText("BRIEFING: long audit session"))
	a := newTestAgent(t, fm, reg)
	a.cfg.Compactor = NewCompactor(fm, 1000)
	a.cfg.Compactor.keepRecent = 2
	a.cfg.Compactor.keepTarget = 2
	var compacted []CompactStat
	a.cfg.OnCompact = func(s CompactStat) { compacted = append(compacted, s) }
	a.Session.Messages = altHistory(12) // enough older history to summarize

	before := len(a.Session.Messages)
	if _, err := a.Turn(context.Background(), "wrap up", ""); err != nil {
		t.Fatal(err)
	}
	if len(compacted) != 1 || compacted[0].Kind != "summary" {
		t.Fatalf("an over-threshold turn must auto-compact once via the summary path, got %+v", compacted)
	}
	if len(a.Session.Messages) >= before {
		t.Fatalf("auto-compaction must durably shrink the persisted session, %d→%d", before, len(a.Session.Messages))
	}
	if !strings.Contains(a.Session.Messages[0].Text(), "BRIEFING") {
		t.Fatalf("the persisted session must begin with the summary, got %q", a.Session.Messages[0].Text())
	}
	// The rewrite is persisted, not just in memory.
	loaded, err := a.Store.Load(a.Session.ID)
	if err != nil || !strings.Contains(loaded.Messages[0].Text(), "BRIEFING") {
		t.Fatalf("compacted session must be saved, err=%v", err)
	}

	// Below threshold: nothing fires.
	light := asstText("ok.")
	light.Usage = &Usage{PromptTokens: 100, TotalTokens: 110}
	fm2 := newFakeModel(light)
	b := newTestAgent(t, fm2, reg)
	b.cfg.Compactor = NewCompactor(fm2, 1000)
	b.cfg.OnCompact = func(CompactStat) { t.Fatal("a light turn must not auto-compact") }
	b.Session.Messages = altHistory(12)
	n := len(b.Session.Messages)
	if _, err := b.Turn(context.Background(), "quick one", ""); err != nil {
		t.Fatal(err)
	}
	if len(b.Session.Messages) != n+2 {
		t.Fatalf("light turn must only append its own messages, %d→%d", n, len(b.Session.Messages))
	}
}

// TestShouldAutoCompactBoundary pins the trigger arithmetic and nil-safety.
func TestShouldAutoCompactBoundary(t *testing.T) {
	c := NewCompactor(newFakeModel(), 1000)
	if c.ShouldAutoCompact(699) {
		t.Fatal("below 70% of the window must not trigger")
	}
	if !c.ShouldAutoCompact(700) {
		t.Fatal("at 70% of the window must trigger")
	}
	if c.ShouldAutoCompact(0) {
		t.Fatal("zero usage (no billing data) must not trigger")
	}
	var nilC *Compactor
	if nilC.ShouldAutoCompact(999999) {
		t.Fatal("nil compactor must be a safe no-op")
	}
}
