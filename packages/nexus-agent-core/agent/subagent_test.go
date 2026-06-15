package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// childRegistry builds a one-tool registry for a subagent.
func childRegistry(tools ...Tool) *Registry {
	r := NewRegistry()
	for _, t := range tools {
		r.Register(t)
	}
	return r
}

func baseSpec(model Model, reg *Registry) SubagentSpec {
	return SubagentSpec{System: "you are a child", Task: "do the thing", Registry: reg, Model: model, MaxTurns: 8}
}

// TestRunSubagent_FinalAnswer is the happy path: a child that answers without
// tools returns the answer as its summary with HaltNone and accumulated usage.
func TestRunSubagent_FinalAnswer(t *testing.T) {
	model := newFakeModel(&ModelResponse{Message: TextMessage(RoleAssistant, "the answer"), StopReason: StopEndTurn, Usage: &Usage{TotalTokens: 12}})
	res, err := RunSubagent(context.Background(), baseSpec(model, childRegistry()))
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if res.Halt != HaltNone || res.Summary != "the answer" {
		t.Fatalf("halt=%v summary=%q; want HaltNone / 'the answer'", res.Halt, res.Summary)
	}
	if res.Usage.TotalTokens != 12 {
		t.Errorf("usage = %d; want 12 accumulated", res.Usage.TotalTokens)
	}
}

// TestRunSubagent_DepthOneRejectsDispatchTool pins FR-51 depth-1: a child registry
// containing the dispatch tool is rejected — a subagent can never spawn another.
func TestRunSubagent_DepthOneRejectsDispatchTool(t *testing.T) {
	reg := childRegistry(&stubTool{name: DispatchToolName})
	_, err := RunSubagent(context.Background(), baseSpec(newFakeModel(), reg))
	if !errors.Is(err, ErrDispatchInChildRegistry) {
		t.Fatalf("err = %v; want ErrDispatchInChildRegistry", err)
	}
}

// TestRunSubagent_TurnCapHalts pins AC-S12-1: a child whose model never stops
// calling tools halts at MaxTurns with the named diagnostic — and gets ONE
// tool-less wrap-up completion so the work done before the cap reaches the
// parent (a mid-loop child otherwise dies summary-less, losing e.g. the
// versionId of a draft it just saved).
func TestRunSubagent_TurnCapHalts(t *testing.T) {
	// Model always asks for the tool again → never a final answer; the 3rd
	// response is consumed by the wrap-up call and becomes the summary.
	loop := []*ModelResponse{asstToolUse("u1", "read", `{}`), asstToolUse("u2", "read", `{}`), asstText("partial: saved draft v-7")}
	model := newFakeModel(loop...)
	spec := baseSpec(model, childRegistry(&stubTool{name: "read"}))
	spec.MaxTurns = 2
	res, err := RunSubagent(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if res.Halt != HaltTurnCap {
		t.Fatalf("halt = %v; want HaltTurnCap", res.Halt)
	}
	if res.Turns != 2 {
		t.Errorf("turns = %d; want 2 (the cap)", res.Turns)
	}
	if !strings.Contains(res.Halt.String(), "turn cap") {
		t.Errorf("halt narrative = %q; want a named turn-cap diagnostic", res.Halt.String())
	}
	// The wrap-up call is tool-less (it cannot start another round) and its
	// text IS the summary the parent receives.
	if model.calls != 3 {
		t.Fatalf("model calls = %d; want 3 (2 rounds + 1 wrap-up)", model.calls)
	}
	if last := model.gotReqs[2]; len(last.Tools) != 0 {
		t.Fatalf("wrap-up request must offer no tools, got %d", len(last.Tools))
	}
	if res.Summary != "partial: saved draft v-7" {
		t.Fatalf("summary = %q; want the wrap-up text", res.Summary)
	}
}

// TestRunSubagent_TokenCapHalts pins the token ceiling: once cumulative usage
// crosses MaxTokens the child stops looping — the only further model call is
// the single tool-less wrap-up whose text becomes the summary.
func TestRunSubagent_TokenCapHalts(t *testing.T) {
	r1 := asstToolUse("u1", "read", `{}`)
	r1.Usage = &Usage{TotalTokens: 100}
	model := newFakeModel(r1, asstText("ran out — versionId v-3 holds the draft"))
	spec := baseSpec(model, childRegistry(&stubTool{name: "read"}))
	spec.MaxTokens = 50 // first call already exceeds → no further tool rounds
	res, err := RunSubagent(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if res.Halt != HaltTokenCap {
		t.Fatalf("halt = %v; want HaltTokenCap", res.Halt)
	}
	if model.calls != 2 {
		t.Errorf("model calls = %d; want 2 (1 round + 1 wrap-up, no further loop)", model.calls)
	}
	if len(model.gotReqs[1].Tools) != 0 {
		t.Fatalf("wrap-up request must offer no tools")
	}
	if res.Summary != "ran out — versionId v-3 holds the draft" {
		t.Fatalf("summary = %q; want the wrap-up text", res.Summary)
	}
}

// TestRunSubagent_ParentCancelHalts pins AC-S12-1 cancellation: cancelling the
// parent context stops the child mid-run with HaltCancelled.
func TestRunSubagent_ParentCancelHalts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the first round
	res, err := RunSubagent(ctx, baseSpec(newFakeModel(asstText("x")), childRegistry()))
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if res.Halt != HaltCancelled {
		t.Fatalf("halt = %v; want HaltCancelled", res.Halt)
	}
}

// TestRunSubagent_DeadlineHalts pins the wall-clock cap: a model that blocks past
// the spec Deadline yields HaltDeadline (distinct from a parent cancel).
func TestRunSubagent_DeadlineHalts(t *testing.T) {
	spec := baseSpec(&blockingModel{released: make(chan struct{})}, childRegistry())
	spec.Deadline = 20 * time.Millisecond
	res, err := RunSubagent(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if res.Halt != HaltDeadline {
		t.Fatalf("halt = %v; want HaltDeadline", res.Halt)
	}
}

// TestRunSubagent_ConfirmBubblesToParent pins T2/AC-S12-3: a confirm-tier tool
// inside the child routes through the PARENT ConfirmFunc with the tool named; a
// denial becomes a non-fatal tool signal the child adapts to, and the typed
// denial is recorded for the summary.
func TestRunSubagent_ConfirmBubblesToParent(t *testing.T) {
	confirmTool := &stubTool{name: "mitigate", tier: TierConfirm}
	model := newFakeModel(asstToolUse("u1", "mitigate", `{"x":1}`), asstText("adapted after denial"))
	spec := baseSpec(model, childRegistry(confirmTool))

	var sawTool string
	spec.Confirm = func(_ context.Context, tool Tool, _ json.RawMessage, _ string) (bool, error) {
		sawTool = tool.Name()
		return false, nil // user declines
	}
	res, err := RunSubagent(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if sawTool != "mitigate" {
		t.Errorf("parent confirm saw %q; want the nested mitigate tool", sawTool)
	}
	if confirmTool.calls != 0 {
		t.Errorf("declined tool ran %d times; want 0 (deny aborts the call)", confirmTool.calls)
	}
	if len(res.Denials) != 1 || res.Denials[0].Kind != DenialUser || res.Denials[0].Tool != "mitigate" {
		t.Errorf("denials = %+v; want one DenialUser on mitigate", res.Denials)
	}
	if res.Halt != HaltNone || res.Summary != "adapted after denial" {
		t.Errorf("child did not adapt: halt=%v summary=%q", res.Halt, res.Summary)
	}
}

// TestRunSubagent_ConfirmTimeoutTypedDistinctly pins T2: a confirm that times out
// (deadline-exceeded) is recorded as DenialTimeout, distinct from a user decline,
// so the summary can name "operator away".
func TestRunSubagent_ConfirmTimeoutTypedDistinctly(t *testing.T) {
	model := newFakeModel(asstToolUse("u1", "mitigate", `{}`), asstText("ok"))
	spec := baseSpec(model, childRegistry(&stubTool{name: "mitigate", tier: TierConfirm}))
	spec.Confirm = func(_ context.Context, _ Tool, _ json.RawMessage, _ string) (bool, error) {
		return false, context.DeadlineExceeded
	}
	res, err := RunSubagent(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if len(res.Denials) != 1 || res.Denials[0].Kind != DenialTimeout {
		t.Errorf("denials = %+v; want one DenialTimeout", res.Denials)
	}
}

// TestRunSubagent_NilConfirmAutoDenies pins fail-safe: with no parent ConfirmFunc,
// a confirm-tier tool is denied (not silently run).
func TestRunSubagent_NilConfirmAutoDenies(t *testing.T) {
	tool := &stubTool{name: "mitigate", tier: TierConfirm}
	model := newFakeModel(asstToolUse("u1", "mitigate", `{}`), asstText("done"))
	spec := baseSpec(model, childRegistry(tool))
	spec.Confirm = nil
	res, err := RunSubagent(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if tool.calls != 0 {
		t.Errorf("confirm-tier tool ran %d times with nil Confirm; want 0", tool.calls)
	}
	if len(res.Denials) != 1 {
		t.Errorf("denials = %+v; want one (auto-denied)", res.Denials)
	}
}

// TestRunSubagent_AllFiveHooksFire pins AC-S12-5: a spec wiring context builder +
// interceptor + output validator + resume transcript + credential observes ALL
// five firing — the hook set S5's NodeExecutor builds on.
func TestRunSubagent_AllFiveHooksFire(t *testing.T) {
	var ctxBuilt, resumed, intercepted, credResolved bool
	validatorCalls := 0

	tool := &stubTool{name: "read"}
	// First answer fails validation, second passes → drives the validator loop AND
	// proves the tool path (interceptor) fired.
	model := newFakeModel(asstToolUse("u1", "read", `{}`), asstText("bad"), asstText("good"))
	spec := baseSpec(model, childRegistry(tool))
	spec.ContextBuilder = func(_ context.Context) ([]Message, error) {
		ctxBuilt = true
		return []Message{TextMessage(RoleUser, "prior context")}, nil
	}
	spec.ResumeTranscript = func() ([]Message, error) {
		resumed = true
		return []Message{TextMessage(RoleAssistant, "earlier turn")}, nil
	}
	spec.ToolInterceptor = func(ctx context.Context, tool Tool, input json.RawMessage, next func() Result) Result {
		intercepted = true
		return next()
	}
	spec.OutputValidator = func(output string) error {
		validatorCalls++
		if output == "good" {
			return nil
		}
		return errors.New("must be 'good'")
	}
	spec.Credential = func(_ context.Context) (string, string, error) {
		credResolved = true
		return "x-nexus-run-token", "rt-live", nil
	}
	res, err := RunSubagent(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if !ctxBuilt || !resumed || !intercepted || !credResolved {
		t.Errorf("hooks fired: ctx=%v resume=%v intercept=%v cred=%v; want all true", ctxBuilt, resumed, intercepted, credResolved)
	}
	if validatorCalls < 2 {
		t.Errorf("validator calls = %d; want ≥2 (reject then accept)", validatorCalls)
	}
	if res.Halt != HaltNone || res.Summary != "good" {
		t.Errorf("halt=%v summary=%q; want the validated 'good' answer", res.Halt, res.Summary)
	}
}

// TestRunSubagent_ValidatorRejectsAfterRetries pins the bounded retry: a child
// whose output never satisfies the contract halts HaltValidatorRejected rather
// than looping forever.
func TestRunSubagent_ValidatorRejectsAfterRetries(t *testing.T) {
	model := newFakeModel(asstText("nope"), asstText("nope"), asstText("nope"), asstText("nope"))
	spec := baseSpec(model, childRegistry())
	spec.OutputValidator = func(string) error { return errors.New("always wrong") }
	res, err := RunSubagent(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if res.Halt != HaltValidatorRejected {
		t.Fatalf("halt = %v; want HaltValidatorRejected", res.Halt)
	}
}

// TestRunSubagent_CredentialThreadedOntoContext pins that the resolved credential
// reaches the run context, readable by the child's transport (S5 run token).
func TestRunSubagent_CredentialThreadedOntoContext(t *testing.T) {
	var seen SubagentCredential
	var ok bool
	tool := &stubTool{name: "read", run: func(json.RawMessage) (Result, error) { return Result{Content: "x"}, nil }}
	spec := baseSpec(newFakeModel(asstToolUse("u1", "read", `{}`), asstText("done")), childRegistry(tool))
	spec.Credential = func(_ context.Context) (string, string, error) { return "x-nexus-run-token", "rt-7", nil }
	spec.ToolInterceptor = func(ctx context.Context, _ Tool, _ json.RawMessage, next func() Result) Result {
		seen, ok = SubagentCredentialFromContext(ctx)
		return next()
	}
	if _, err := RunSubagent(context.Background(), spec); err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if !ok || seen.Name != "x-nexus-run-token" || seen.Value != "rt-7" {
		t.Errorf("credential on ctx = %+v ok=%v; want the resolved run token", seen, ok)
	}
}

// TestRunSubagent_InterceptorCanShortCircuit proves the interceptor controls
// dispatch — it can replace the tool result without calling next (S5 uses this to
// serve a replayed sub-invocation from the blackboard instead of re-executing).
func TestRunSubagent_InterceptorCanShortCircuit(t *testing.T) {
	tool := &stubTool{name: "read"}
	spec := baseSpec(newFakeModel(asstToolUse("u1", "read", `{}`), asstText("done")), childRegistry(tool))
	spec.ToolInterceptor = func(_ context.Context, _ Tool, _ json.RawMessage, _ func() Result) Result {
		return Result{Content: "replayed"} // never calls next
	}
	if _, err := RunSubagent(context.Background(), spec); err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if tool.calls != 0 {
		t.Errorf("tool ran %d times; want 0 (interceptor short-circuited)", tool.calls)
	}
}

// TestRunSubagent_ModelErrorSurfaces pins that a non-context model error ends the
// run with HaltError and the error returned (not swallowed).
func TestRunSubagent_ModelErrorSurfaces(t *testing.T) {
	model := &fakeModel{resps: []*ModelResponse{nil}, errs: []error{errors.New("gateway down")}}
	res, err := RunSubagent(context.Background(), baseSpec(model, childRegistry()))
	if err == nil {
		t.Fatal("want a model error surfaced")
	}
	if res.Halt != HaltError {
		t.Errorf("halt = %v; want HaltError", res.Halt)
	}
}

// TestSubagentHalt_StringCoversAllArms exercises every halt narrative so the
// operator-facing strings are all real (ruling #5 — typed failures).
func TestSubagentHalt_StringCoversAllArms(t *testing.T) {
	for _, h := range []SubagentHalt{HaltNone, HaltTurnCap, HaltTokenCap, HaltDeadline, HaltCancelled, HaltValidatorRejected, HaltError, SubagentHalt(99)} {
		if h.String() == "" {
			t.Errorf("halt %d has an empty narrative", int(h))
		}
	}
}

// TestRunSubagent_NilSpecGuards pins the programmer-error guards.
func TestRunSubagent_NilSpecGuards(t *testing.T) {
	if _, err := RunSubagent(context.Background(), SubagentSpec{Model: newFakeModel()}); err == nil {
		t.Error("nil registry must error")
	}
	if _, err := RunSubagent(context.Background(), SubagentSpec{Registry: childRegistry()}); err == nil {
		t.Error("nil model must error")
	}
}

// TestRunSubagent_HookErrorsHalt pins that a failing ContextBuilder, ResumeTranscript,
// or Credential ends the run with HaltError and the wrapped cause.
func TestRunSubagent_HookErrorsHalt(t *testing.T) {
	cases := map[string]func(*SubagentSpec){
		"context": func(s *SubagentSpec) {
			s.ContextBuilder = func(context.Context) ([]Message, error) { return nil, errors.New("ctx boom") }
		},
		"resume": func(s *SubagentSpec) {
			s.ResumeTranscript = func() ([]Message, error) { return nil, errors.New("resume boom") }
		},
		"credential": func(s *SubagentSpec) {
			s.Credential = func(context.Context) (string, string, error) { return "", "", errors.New("cred boom") }
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			spec := baseSpec(newFakeModel(asstText("x")), childRegistry())
			mut(&spec)
			res, err := RunSubagent(context.Background(), spec)
			if err == nil {
				t.Fatalf("%s hook error must surface", name)
			}
			if res.Halt != HaltError {
				t.Errorf("halt = %v; want HaltError", res.Halt)
			}
		})
	}
}

// TestRunSubagent_UnknownToolIsError pins that a model calling a tool absent from
// the pruned registry gets a tool error (not a crash).
func TestRunSubagent_UnknownToolIsError(t *testing.T) {
	model := newFakeModel(asstToolUse("u1", "ghost", `{}`), asstText("recovered"))
	res, err := RunSubagent(context.Background(), baseSpec(model, childRegistry(&stubTool{name: "read"})))
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if res.Halt != HaltNone {
		t.Errorf("halt = %v; want HaltNone (child adapts to the missing tool)", res.Halt)
	}
}

// TestRunSubagent_ToolErrorIsSignal pins that a tool returning an error becomes an
// IsError tool result the child sees, never a chassis crash.
func TestRunSubagent_ToolErrorIsSignal(t *testing.T) {
	tool := &stubTool{name: "read", run: func(json.RawMessage) (Result, error) { return Result{}, errors.New("disk full") }}
	var sawError bool
	model := newFakeModel(asstToolUse("u1", "read", `{}`), asstText("ok"))
	spec := baseSpec(model, childRegistry(tool))
	spec.OnToolEnd = func(_ string, _ json.RawMessage, isError bool) { sawError = sawError || isError }
	if _, err := RunSubagent(context.Background(), spec); err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if !sawError {
		t.Error("a tool error must surface as an IsError result to the child")
	}
}

// TestRunSubagent_PartialSummaryOnHalt pins that a halt surfaces the latest
// assistant text as the best partial summary.
func TestRunSubagent_PartialSummaryOnHalt(t *testing.T) {
	// Each round: assistant emits text AND a tool call (never a clean final) → turn cap.
	withText := func(text, id string) *ModelResponse {
		return &ModelResponse{Message: Message{Role: RoleAssistant, Blocks: []Block{
			{Type: BlockText, Text: text},
			{Type: BlockToolUse, ID: id, ToolName: "read", Input: json.RawMessage(`{}`)},
		}}, StopReason: StopToolUse}
	}
	model := newFakeModel(withText("working on it", "u1"), withText("still going", "u2"))
	spec := baseSpec(model, childRegistry(&stubTool{name: "read"}))
	spec.MaxTurns = 2
	res, err := RunSubagent(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if res.Halt != HaltTurnCap || res.Summary != "still going" {
		t.Errorf("halt=%v summary=%q; want HaltTurnCap with the latest partial text", res.Halt, res.Summary)
	}
}

// TestRunSubagent_DefaultsAndObservers closes the chassis's remaining branches in
// one path: MaxTurns=0 falls back to DefaultStepCap, a Compactor bounds the view,
// and OnToolStart fires on a tool dispatch.
func TestRunSubagent_DefaultsAndObservers(t *testing.T) {
	tool := &stubTool{name: "read"}
	model := newFakeModel(asstToolUse("u1", "read", `{}`), asstText("done"))
	spec := SubagentSpec{
		System: "child", Task: "go", Registry: childRegistry(tool), Model: model,
		MaxTurns:  0, // → DefaultStepCap
		Compactor: NewCompactor(model, 0),
	}
	var started string
	spec.OnToolStart = func(name string, _ json.RawMessage) { started = name }
	res, err := RunSubagent(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if res.Halt != HaltNone || res.Summary != "done" {
		t.Errorf("halt=%v summary=%q; want clean completion", res.Halt, res.Summary)
	}
	if started != "read" {
		t.Errorf("OnToolStart saw %q; want read", started)
	}
}

// TestRunSubagent_CompactorTrimsLargeHistory forces the model-view trim branch: a
// large resumed transcript exceeds the compactor budget, so FitToWindow trims the
// child's view (the run still completes — only the model view is bounded).
func TestRunSubagent_CompactorTrimsLargeHistory(t *testing.T) {
	big := strings.Repeat("x", 400_000) // well past the fallback trim budget
	model := newFakeModel(asstText("done"))
	spec := SubagentSpec{
		System: "child", Task: "go", Registry: childRegistry(), Model: model,
		MaxTurns:         4,
		Compactor:        NewCompactor(model, 0),
		ResumeTranscript: func() ([]Message, error) { return []Message{TextMessage(RoleAssistant, big)}, nil },
	}
	res, err := RunSubagent(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if res.Halt != HaltNone {
		t.Errorf("halt = %v; want HaltNone (trim bounds the view, run still completes)", res.Halt)
	}
}

// TestRunSubagent_OnUnknownToolFires pins the FR-13b runtime leg seam: when the
// model requests a tool outside the child's curated registry, the attempt is
// rejected (non-fatal) AND surfaced via OnUnknownTool so S5 can record it.
func TestRunSubagent_OnUnknownToolFires(t *testing.T) {
	var gotName string
	model := newFakeModel(asstToolUse("u1", "ghost_tool", `{"a":1}`), asstText("done"))
	spec := baseSpec(model, childRegistry(&stubTool{name: "read"})) // registry lacks "ghost_tool"
	spec.OnUnknownTool = func(name string, input json.RawMessage) { gotName = name }
	res, err := RunSubagent(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if gotName != "ghost_tool" {
		t.Fatalf("OnUnknownTool name = %q, want ghost_tool", gotName)
	}
	if res.Halt != HaltNone {
		t.Fatalf("an undeclared-tool attempt must be non-fatal, got halt %v", res.Halt)
	}
}
