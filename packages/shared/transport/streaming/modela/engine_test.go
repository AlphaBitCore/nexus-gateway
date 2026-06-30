package modela

import (
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"

	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// fakeUnit is the engine's opaque unit for tests: it carries the redactable text
// the prescan/confirm see, the content/transport byte sizes the tail-window math
// uses, and a done flag.
type fakeUnit struct {
	id      int
	text    string
	content int
	bytes   int
	done    bool
}

// fakeSubstrate is a fully-recording Substrate[fakeUnit]. It feeds a fixed unit
// queue, lets a test inject prescan/confirm verdicts and delivery/next errors, and
// records every engine-driven operation so a test can assert the observable
// algorithm behaviour (delivery order, escalation hand-off, audit stamps).
type fakeSubstrate struct {
	units []fakeUnit
	idx   int

	// nextErr, when set, is returned by Next once idx reaches nextErrAt (instead of
	// the queued unit / EOF) to exercise the terminal-error path.
	nextErr   error
	nextErrAt int

	prescanFn    func(content []byte) bool
	confirmFn    func(content string) *hookcore.CompliancePipelineResult
	deliverErrAt int // unit id whose Deliver returns deliverErr; -1 disables
	deliverErr   error

	// recorded observations
	delivered       []int
	terminalCalled  int
	approveEOF      int
	confirmApproved int
	escalated       bool
	escalateHeld    []int
	escalateRes     *hookcore.CompliancePipelineResult
	onErrorCalled   int
	onErrorErr      error
	confirmCalls    []string
	prescanCalls    []string
}

func newFake(units []fakeUnit) *fakeSubstrate {
	return &fakeSubstrate{units: units, nextErrAt: -1, deliverErrAt: -1}
}

func (f *fakeSubstrate) Next(_ context.Context) (fakeUnit, error) {
	if f.nextErr != nil && f.idx == f.nextErrAt {
		return fakeUnit{}, f.nextErr
	}
	if f.idx >= len(f.units) {
		return fakeUnit{}, io.EOF
	}
	u := f.units[f.idx]
	f.idx++
	return u, nil
}

func (f *fakeSubstrate) AppendRedactableText(dst []byte, u fakeUnit) []byte {
	return append(dst, u.text...)
}
func (f *fakeSubstrate) UnitBytes(u fakeUnit) int    { return u.bytes }
func (f *fakeSubstrate) ContentBytes(u fakeUnit) int { return u.content }
func (f *fakeSubstrate) IsDone(u fakeUnit) bool      { return u.done }

func (f *fakeSubstrate) Deliver(_ context.Context, u fakeUnit) error {
	if f.deliverErr != nil && u.id == f.deliverErrAt {
		return f.deliverErr
	}
	f.delivered = append(f.delivered, u.id)
	return nil
}

func (f *fakeSubstrate) DeliverTerminal(_ context.Context) error { f.terminalCalled++; return nil }

func (f *fakeSubstrate) Prescan(content []byte) bool {
	f.prescanCalls = append(f.prescanCalls, string(content))
	if f.prescanFn == nil {
		return false
	}
	return f.prescanFn(content)
}

func (f *fakeSubstrate) Confirm(_ context.Context, content string) *hookcore.CompliancePipelineResult {
	f.confirmCalls = append(f.confirmCalls, content)
	if f.confirmFn == nil {
		return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
	}
	return f.confirmFn(content)
}

func (f *fakeSubstrate) Escalate(_ context.Context, held []fakeUnit, res *hookcore.CompliancePipelineResult) error {
	f.escalated = true
	for _, u := range held {
		f.escalateHeld = append(f.escalateHeld, u.id)
	}
	f.escalateRes = res
	return nil
}

func (f *fakeSubstrate) OnConfirmApproved(_ *hookcore.CompliancePipelineResult) { f.confirmApproved++ }
func (f *fakeSubstrate) OnApproveEOF()                                          { f.approveEOF++ }
func (f *fakeSubstrate) OnError(_ context.Context, err error) error {
	f.onErrorCalled++
	f.onErrorErr = err
	return err
}

func reject() *hookcore.CompliancePipelineResult {
	return &hookcore.CompliancePipelineResult{Decision: hookcore.RejectHard}
}
func modify() *hookcore.CompliancePipelineResult {
	return &hookcore.CompliancePipelineResult{Decision: hookcore.Modify}
}
func approve() *hookcore.CompliancePipelineResult {
	return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
}

func eq(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestNoPrescanHit_AllDeliveredApproveAtEOF: with a prescan that never matches and
// content under the tail window, every unit is held until EOF, then flushed in
// order; no confirm runs, so the engine stamps OnApproveEOF and emits the terminal.
func TestNoPrescanHit_AllDeliveredApproveAtEOF(t *testing.T) {
	f := newFake([]fakeUnit{
		{id: 1, text: "a", content: 1, bytes: 1},
		{id: 2, text: "b", content: 1, bytes: 1},
		{id: 3, text: "c", content: 1, bytes: 1, done: true},
	})
	if err := Run(context.Background(), f, Config{}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !eq(f.delivered, []int{1, 2, 3}) {
		t.Fatalf("delivered order = %v, want [1 2 3]", f.delivered)
	}
	if f.escalated {
		t.Fatal("escalated unexpectedly")
	}
	if len(f.confirmCalls) != 0 {
		t.Fatalf("confirm ran %d times, want 0", len(f.confirmCalls))
	}
	if f.approveEOF != 1 {
		t.Fatalf("OnApproveEOF called %d times, want 1", f.approveEOF)
	}
	if f.terminalCalled != 1 {
		t.Fatalf("DeliverTerminal called %d times, want 1", f.terminalCalled)
	}
}

// TestEOFWithoutDoneMarker: a stream that ends by Next returning io.EOF (no
// explicit done unit — some wires omit a terminator) still flushes the held tail,
// stamps OnApproveEOF, and emits the terminal frame.
func TestEOFWithoutDoneMarker(t *testing.T) {
	f := newFake([]fakeUnit{
		{id: 1, text: "a", content: 1, bytes: 1},
		{id: 2, text: "b", content: 1, bytes: 1}, // no done flag → loop exits via io.EOF
	})
	if err := Run(context.Background(), f, Config{}); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !eq(f.delivered, []int{1, 2}) {
		t.Fatalf("delivered = %v, want [1 2]", f.delivered)
	}
	if f.approveEOF != 1 || f.terminalCalled != 1 {
		t.Fatalf("approveEOF=%d terminal=%d, want 1/1", f.approveEOF, f.terminalCalled)
	}
}

// TestPrescanFalsePositive_ResumesStreaming: prescan hits but confirm approves, so
// the engine stamps OnConfirmApproved and resumes real-time streaming — no
// escalation, and because a confirm DID run, OnApproveEOF is NOT stamped at EOF.
func TestPrescanFalsePositive_ResumesStreaming(t *testing.T) {
	f := newFake([]fakeUnit{
		{id: 1, text: "secret", content: 6, bytes: 6},
		{id: 2, text: "more", content: 4, bytes: 4, done: true},
	})
	f.prescanFn = func([]byte) bool { return true }
	f.confirmFn = func(string) *hookcore.CompliancePipelineResult { return approve() }
	if err := Run(context.Background(), f, Config{TailWindowBytes: 1024, MaxBufferBytes: 1 << 20}); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if f.escalated {
		t.Fatal("escalated on an approve confirm")
	}
	if f.confirmApproved == 0 {
		t.Fatal("OnConfirmApproved never stamped on a false positive")
	}
	if f.approveEOF != 0 {
		t.Fatalf("OnApproveEOF stamped %d times after a confirm ran, want 0", f.approveEOF)
	}
	if !eq(f.delivered, []int{1, 2}) {
		t.Fatalf("delivered = %v, want [1 2]", f.delivered)
	}
}

// TestConfirmedReject_Escalates: a prescan hit confirmed as RejectHard hands the
// held units to Escalate and stops the engine — the held tail is NOT delivered raw.
func TestConfirmedReject_Escalates(t *testing.T) {
	f := newFake([]fakeUnit{
		{id: 1, text: "card 4111", content: 9, bytes: 9},
		{id: 2, text: "1111", content: 4, bytes: 4, done: true},
	})
	f.prescanFn = func([]byte) bool { return true }
	f.confirmFn = func(string) *hookcore.CompliancePipelineResult { return reject() }
	// PrescanBatchBytes:1 exercises per-unit scan timing — the confirm fires as soon as
	// the first unit's content is scanned, so only unit 1 is held at escalation.
	if err := Run(context.Background(), f, Config{TailWindowBytes: 1024, MaxBufferBytes: 1 << 20, PrescanBatchBytes: 1}); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !f.escalated {
		t.Fatal("did not escalate on a confirmed reject")
	}
	if !eq(f.escalateHeld, []int{1}) {
		t.Fatalf("escalate held = %v, want [1] (the single held unit)", f.escalateHeld)
	}
	if len(f.delivered) != 0 {
		t.Fatalf("delivered %v before escalation, want none (held under window)", f.delivered)
	}
	if f.escalateRes == nil || f.escalateRes.Decision != hookcore.RejectHard {
		t.Fatalf("escalate result = %+v, want RejectHard", f.escalateRes)
	}
	if f.terminalCalled != 0 {
		t.Fatal("DeliverTerminal ran on the engine path after escalation handed off")
	}
}

// TestConfirmedModify_Escalates: a Modify decision escalates exactly like a block —
// a redact that cannot be applied losslessly on the live wire goes to buffer-to-end.
func TestConfirmedModify_Escalates(t *testing.T) {
	f := newFake([]fakeUnit{{id: 1, text: "ssn 078051120", content: 13, bytes: 13, done: true}})
	f.prescanFn = func([]byte) bool { return true }
	f.confirmFn = func(string) *hookcore.CompliancePipelineResult { return modify() }
	if err := Run(context.Background(), f, Config{}); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !f.escalated || f.escalateRes.Decision != hookcore.Modify {
		t.Fatalf("modify did not escalate: escalated=%v res=%+v", f.escalated, f.escalateRes)
	}
}

// TestTailWindowReleasesPrefix: with a small tail window and a never-matching
// prescan, units beyond the window are delivered in real time (only the trailing
// window stays held until EOF) — proving the bounded-tail real-time behaviour.
func TestTailWindowReleasesPrefix(t *testing.T) {
	f := newFake([]fakeUnit{
		{id: 1, text: "aaa", content: 3, bytes: 3},
		{id: 2, text: "bbb", content: 3, bytes: 3},
		{id: 3, text: "ccc", content: 3, bytes: 3, done: true},
	})
	// window = 3 bytes: after unit 2 the held content (6) exceeds 3, so unit 1 is
	// released; after unit 3 the held content (now units 2+3 = 6) exceeds 3 so unit 2
	// releases. The final held tail (unit 3) flushes at EOF.
	if err := Run(context.Background(), f, Config{TailWindowBytes: 3, MaxBufferBytes: 1 << 20}); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !eq(f.delivered, []int{1, 2, 3}) {
		t.Fatalf("delivered = %v, want [1 2 3] in order", f.delivered)
	}
}

// TestMaxBufferReleasesOnByteCeiling: a non-content-heavy unit (content 0 but large
// bytes) does not trip the content window, but the held-bytes ceiling forces a
// release so memory stays bounded during a long reasoning phase.
func TestMaxBufferReleasesOnByteCeiling(t *testing.T) {
	f := newFake([]fakeUnit{
		{id: 1, text: "", content: 0, bytes: 100},
		{id: 2, text: "", content: 0, bytes: 100, done: true},
	})
	if err := Run(context.Background(), f, Config{TailWindowBytes: 1 << 20, MaxBufferBytes: 150}); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	// After unit 2, heldBytes (200) > 150 → release unit 1; unit 2 flushes at EOF.
	if !eq(f.delivered, []int{1, 2}) {
		t.Fatalf("delivered = %v, want [1 2]", f.delivered)
	}
}

// TestWindowedConfirmDedup: a persistent prescan hit confirms only on NEW content
// since the last confirm (confirmedLen gate), not once per unit — so a benign
// false-positive trigger is not re-confirmed until fresh bytes arrive.
func TestWindowedConfirmDedup(t *testing.T) {
	f := newFake([]fakeUnit{
		{id: 1, text: "x", content: 1, bytes: 1},
		{id: 2, text: "", content: 0, bytes: 0}, // no new scan content
		{id: 3, text: "y", content: 1, bytes: 1, done: true},
	})
	f.prescanFn = func([]byte) bool { return true }
	f.confirmFn = func(string) *hookcore.CompliancePipelineResult { return approve() }
	// PrescanBatchBytes:1 exercises per-unit scan timing so the dedup is observable:
	// unit 2 adds no scan bytes, so scanThrough is a no-op there.
	if err := Run(context.Background(), f, Config{TailWindowBytes: 1024, MaxBufferBytes: 1 << 20, PrescanBatchBytes: 1}); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	// Unit 2 added no scan bytes, so len(scanBuf) did not exceed scannedLen → no
	// second confirm. Confirms fire only on units 1 and 3.
	if len(f.confirmCalls) != 2 {
		t.Fatalf("confirm ran %d times, want 2 (only on new content)", len(f.confirmCalls))
	}
}

// TestNilConfirmTreatedAsApprove: a nil confirm result is treated as approve — no
// escalation. But a confirm DID execute, so the audit records it as an evaluated
// approve (OnConfirmApproved), NOT as "no hook ran" (OnApproveEOF), preserving the
// SIEM distinction between "evaluated-approved" and "no response hook configured".
func TestNilConfirmTreatedAsApprove(t *testing.T) {
	f := newFake([]fakeUnit{{id: 1, text: "z", content: 1, bytes: 1, done: true}})
	f.prescanFn = func([]byte) bool { return true }
	f.confirmFn = func(string) *hookcore.CompliancePipelineResult { return nil }
	if err := Run(context.Background(), f, Config{}); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if f.escalated {
		t.Fatal("nil confirm result must not escalate")
	}
	if f.confirmApproved != 1 {
		t.Fatalf("OnConfirmApproved = %d on a nil-as-approve result, want 1 (a confirm ran)", f.confirmApproved)
	}
	// A confirm ran → anyConfirm true → EOF must NOT re-stamp as no-hook approve.
	if f.approveEOF != 0 {
		t.Fatalf("OnApproveEOF = %d after a confirm ran, want 0", f.approveEOF)
	}
}

// TestBlockSoftMaskedRedact_Escalates pins the security gate fix: the pipeline
// aggregator ranks a co-firing soft-block ABOVE a redact, so a confirm that found
// PII and computed redact spans can surface as a BlockSoft decision. Keying the
// escalate gate on the enforcing ACTION (BlockSoft→block) — not the raw decision
// enum — escalates it instead of streaming the masked redact raw.
func TestBlockSoftMaskedRedact_Escalates(t *testing.T) {
	f := newFake([]fakeUnit{{id: 1, text: "pii here", content: 8, bytes: 8, done: true}})
	f.prescanFn = func([]byte) bool { return true }
	f.confirmFn = func(string) *hookcore.CompliancePipelineResult {
		// A redact hook's spans masked behind a soft-block aggregated decision.
		return &hookcore.CompliancePipelineResult{Decision: hookcore.BlockSoft}
	}
	if err := Run(context.Background(), f, Config{}); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !f.escalated {
		t.Fatal("BlockSoft (block action) was not escalated — PII would stream raw")
	}
	if len(f.delivered) != 0 {
		t.Fatalf("delivered %v before escalation on a block-action confirm, want none", f.delivered)
	}
}

// TestMaxBufferContentEviction_Escalates pins the second leak fix: a content unit
// whose value is not yet complete must NOT be force-delivered raw under memory
// pressure. Here a small content unit is followed by reasoning (content==0, large
// bytes) that trips the MaxBufferBytes ceiling while the content window is NOT
// exceeded — the engine escalates rather than flushing the content unit raw.
func TestMaxBufferContentEviction_Escalates(t *testing.T) {
	f := newFake([]fakeUnit{
		{id: 1, text: "SSN 078-05-", content: 11, bytes: 11}, // incomplete sensitive value
		{id: 2, text: "", content: 0, bytes: 500},            // reasoning: trips the byte ceiling
	})
	f.prescanFn = func([]byte) bool { return false } // isolate the memory-pressure path
	if err := Run(context.Background(), f, Config{TailWindowBytes: 1 << 20, MaxBufferBytes: 150}); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !f.escalated {
		t.Fatal("memory-pressure eviction of an incomplete content unit did not escalate — sub-window leak")
	}
	if !eq(f.escalateHeld, []int{1, 2}) {
		t.Fatalf("escalate held = %v, want [1 2]", f.escalateHeld)
	}
	if f.escalateRes != nil {
		t.Fatal("memory-pressure escalation must pass a nil triggering result")
	}
	if len(f.delivered) != 0 {
		t.Fatalf("delivered %v under memory pressure, want none (content held)", f.delivered)
	}
}

// TestReasoningEvictionUnderMemoryPressure: a non-content (reasoning) unit at the
// front IS delivered raw under the byte ceiling — reasoning is never redacted, so
// evicting it raw is safe and keeps memory bounded without forcing escalation.
func TestReasoningEvictionUnderMemoryPressure(t *testing.T) {
	f := newFake([]fakeUnit{
		{id: 1, text: "", content: 0, bytes: 100}, // reasoning at front
		{id: 2, text: "", content: 0, bytes: 100, done: true},
	})
	f.prescanFn = func([]byte) bool { return false }
	if err := Run(context.Background(), f, Config{TailWindowBytes: 1 << 20, MaxBufferBytes: 150}); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if f.escalated {
		t.Fatal("reasoning eviction must not escalate")
	}
	if !eq(f.delivered, []int{1, 2}) {
		t.Fatalf("delivered = %v, want [1 2] (reasoning evicted raw)", f.delivered)
	}
}

// TestPrescanWindowedToHeldContent locks the O(N) windowed-prescan contract: after a
// unit is released past the tail window, the next prescan sees ONLY the still-held
// content (from the released boundary), not the whole accumulated buffer — proving
// the lookbehind anchors on the oldest held unit, not a growing full-buffer scan.
func TestPrescanWindowedToHeldContent(t *testing.T) {
	f := newFake([]fakeUnit{
		{id: 1, text: "aaa", content: 3, bytes: 3},
		{id: 2, text: "bbb", content: 3, bytes: 3},
		{id: 3, text: "ccc", content: 3, bytes: 3, done: true},
	})
	f.prescanFn = func([]byte) bool { return false }
	if err := Run(context.Background(), f, Config{TailWindowBytes: 3, MaxBufferBytes: 1 << 20, PrescanBatchBytes: 1}); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	// Prescans: u1 sees "aaa"; u2 sees "aaabbb"; after u1 releases (deliveredScanLen=3),
	// u3 sees only "bbbccc" — NOT the whole "aaabbbccc".
	if len(f.prescanCalls) != 3 {
		t.Fatalf("prescan ran %d times, want 3: %v", len(f.prescanCalls), f.prescanCalls)
	}
	if f.prescanCalls[2] != "bbbccc" {
		t.Fatalf("third prescan saw %q, want windowed %q (delivered prefix excluded)", f.prescanCalls[2], "bbbccc")
	}
}

// TestSeparatorAccounting_TailUsesContentNotTextLen pins that the tail window is
// sized by separator-free ContentBytes, NOT by appended scan-text length. Three
// units of content 2 / scan-text 3 (a "\n" channel separator) under window 5: held
// content after two units is 4 ≤ 5 (nothing released), but if the window were
// (wrongly) sized by scan-text length it would be 6 > 5 and evict unit 1 early. The
// third unit triggers an escalation; asserting the escalation still holds [1 2 3]
// (and nothing was delivered) discriminates the two accountings — a text-length
// window would have released unit 1 and the escalation would see only [2 3].
func TestSeparatorAccounting_TailUsesContentNotTextLen(t *testing.T) {
	f := newFake([]fakeUnit{
		{id: 1, text: "ab\n", content: 2, bytes: 3},
		{id: 2, text: "cd\n", content: 2, bytes: 3},
		{id: 3, text: "ef\n", content: 2, bytes: 3, done: true},
	})
	// Prescan misses until the third unit's content arrives, then confirm blocks.
	f.prescanFn = func(b []byte) bool { return strings.Contains(string(b), "ef") }
	f.confirmFn = func(string) *hookcore.CompliancePipelineResult { return reject() }
	// PrescanBatchBytes:1 scans per unit so this pins the content-vs-text-length window
	// accounting (not batch timing): unit 1 stays held under the content-sized window.
	if err := Run(context.Background(), f, Config{TailWindowBytes: 5, MaxBufferBytes: 1 << 20, PrescanBatchBytes: 1}); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !f.escalated {
		t.Fatal("third-unit confirm did not escalate")
	}
	if !eq(f.escalateHeld, []int{1, 2, 3}) {
		t.Fatalf("escalate held = %v, want [1 2 3] — a text-length window would have evicted unit 1", f.escalateHeld)
	}
	if len(f.delivered) != 0 {
		t.Fatalf("delivered %v, want none (all held under content-sized window)", f.delivered)
	}
}

// TestNextError_OnError: a non-EOF Next error routes through OnError and the engine
// returns that error without delivering a terminal frame.
func TestNextError_OnError(t *testing.T) {
	sentinel := errors.New("upstream exploded")
	f := newFake([]fakeUnit{{id: 1, text: "a", content: 1, bytes: 1}})
	f.nextErr = sentinel
	f.nextErrAt = 1 // first unit succeeds, second Next errors
	err := Run(context.Background(), f, Config{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run error = %v, want %v", err, sentinel)
	}
	if f.onErrorCalled != 1 || !errors.Is(f.onErrorErr, sentinel) {
		t.Fatalf("OnError calls=%d err=%v", f.onErrorCalled, f.onErrorErr)
	}
	if f.terminalCalled != 0 {
		t.Fatal("DeliverTerminal must not run after a terminal Next error")
	}
}

// TestDeliverError_StopsDuringRelease: a Deliver error during tail release stops the
// engine and surfaces the error (the substrate has recorded its own terminal state).
func TestDeliverError_StopsDuringRelease(t *testing.T) {
	derr := errors.New("client gone")
	f := newFake([]fakeUnit{
		{id: 1, text: "aaa", content: 3, bytes: 3},
		{id: 2, text: "bbb", content: 3, bytes: 3},
		{id: 3, text: "ccc", content: 3, bytes: 3, done: true},
	})
	f.deliverErr = derr
	f.deliverErrAt = 1 // unit 1 is the first released by the small window
	err := Run(context.Background(), f, Config{TailWindowBytes: 3, MaxBufferBytes: 1 << 20})
	if !errors.Is(err, derr) {
		t.Fatalf("Run error = %v, want %v", err, derr)
	}
	if f.terminalCalled != 0 {
		t.Fatal("DeliverTerminal must not run after a delivery error")
	}
}

// TestDeliverError_StopsAtFinalFlush: a Deliver error while flushing the held tail at
// EOF also stops the engine before the terminal frame.
func TestDeliverError_StopsAtFinalFlush(t *testing.T) {
	derr := errors.New("client gone at flush")
	f := newFake([]fakeUnit{{id: 7, text: "a", content: 1, bytes: 1, done: true}})
	f.deliverErr = derr
	f.deliverErrAt = 7
	err := Run(context.Background(), f, Config{})
	if !errors.Is(err, derr) {
		t.Fatalf("Run error = %v, want %v", err, derr)
	}
	if f.terminalCalled != 0 {
		t.Fatal("DeliverTerminal must not run after a final-flush delivery error")
	}
}

// allocSubstrate is a zero-overhead Substrate[int] whose methods neither record nor
// allocate, so testing.AllocsPerRun over Run measures ONLY the engine's own
// allocations (not test-fake bookkeeping). It streams `units` one-byte content units.
type allocSubstrate struct {
	units int
	idx   int
}

func (a *allocSubstrate) Next(context.Context) (int, error) {
	if a.idx >= a.units {
		return 0, io.EOF
	}
	a.idx++
	return a.idx, nil
}
func (a *allocSubstrate) AppendRedactableText(dst []byte, _ int) []byte { return append(dst, 'x') }
func (a *allocSubstrate) UnitBytes(int) int                             { return 1 }
func (a *allocSubstrate) ContentBytes(int) int                          { return 1 }
func (a *allocSubstrate) IsDone(int) bool                               { return false }
func (a *allocSubstrate) Deliver(context.Context, int) error            { return nil }
func (a *allocSubstrate) DeliverTerminal(context.Context) error         { return nil }
func (a *allocSubstrate) Prescan([]byte) bool                           { return false }
func (a *allocSubstrate) Confirm(context.Context, string) *hookcore.CompliancePipelineResult {
	return nil
}
func (a *allocSubstrate) Escalate(context.Context, []int, *hookcore.CompliancePipelineResult) error {
	return nil
}
func (a *allocSubstrate) OnConfirmApproved(*hookcore.CompliancePipelineResult) {}
func (a *allocSubstrate) OnApproveEOF()                                        {}
func (a *allocSubstrate) OnError(_ context.Context, err error) error           { return err }

// TestRunHotPathAllocationsAreAmortized locks the perf guarantee behind the
// append-style AppendRedactableText seam: per-unit work allocates nothing; only the
// engine's three growable buffers (scanBuf / held / heldScanLens) allocate, and only
// on amortized doubling — O(log units), far below one-per-unit. A regression to a
// per-unit fresh-[]byte return (the original RedactableText shape) would push the
// count toward `units`. With prescan false, no confirm runs, so string(scanBuf) is
// never paid here.
func TestRunHotPathAllocationsAreAmortized(t *testing.T) {
	const units = 256
	avg := testing.AllocsPerRun(50, func() {
		s := &allocSubstrate{units: units}
		_ = Run(context.Background(), s, Config{TailWindowBytes: 1 << 20, MaxBufferBytes: 1 << 20})
	})
	if avg > 64 {
		t.Fatalf("Run allocated %.0f times for %d units; expected amortized O(log n) growth, not per-unit", avg, units)
	}
}

// TestConfigDefaults: zero config takes the package defaults; explicit values win.
func TestConfigDefaults(t *testing.T) {
	got := Config{}.withDefaults()
	if got.TailWindowBytes != defaultTailWindowBytes || got.MaxBufferBytes != defaultMaxBufferBytes {
		t.Fatalf("defaults = %+v, want tail=%d max=%d", got, defaultTailWindowBytes, defaultMaxBufferBytes)
	}
	if got.PrescanBatchBytes != defaultPrescanBatchBytes {
		t.Fatalf("PrescanBatchBytes default = %d, want %d", got.PrescanBatchBytes, defaultPrescanBatchBytes)
	}
	if got.MaxPatternBytes != defaultMaxPatternBytes {
		t.Fatalf("MaxPatternBytes default = %d, want %d", got.MaxPatternBytes, defaultMaxPatternBytes)
	}
	custom := Config{TailWindowBytes: 7, MaxBufferBytes: 9}.withDefaults()
	if custom.TailWindowBytes != 7 || custom.MaxBufferBytes != 9 {
		t.Fatalf("explicit config overwritten: %+v", custom)
	}
	// MaxPatternBytes is clamped below TailWindowBytes (the lookahead never needs to reach
	// past the held window — a wider value is the disclosed over-window surface).
	if custom.MaxPatternBytes != custom.TailWindowBytes-1 {
		t.Fatalf("MaxPatternBytes clamp = %d, want %d (TailWindowBytes-1)", custom.MaxPatternBytes, custom.TailWindowBytes-1)
	}
}

// TestCompactionWindowedConfirm proves the #12 fix keeps detection sound: with a small
// tail window, delivering several units advances deliveredScanLen past the window so
// scanBuf is compacted (the delivered prefix dropped + offsets rebased). A later prescan
// hit must STILL fire a confirm (the `len(scanBuf) > confirmedLen` gate not wrongly
// skipped after the rebase — the leak class), and the confirm must see only the held
// window (not the compacted-away delivered prefix).
func TestCompactionWindowedConfirm(t *testing.T) {
	f := newFake([]fakeUnit{
		{id: 0, text: "aaa", bytes: 3, content: 3},
		{id: 1, text: "bbb", bytes: 3, content: 3},
		{id: 2, text: "ccc", bytes: 3, content: 3},
		{id: 3, text: "ddd", bytes: 3, content: 3, done: true},
	})
	f.prescanFn = func(c []byte) bool { return strings.Contains(string(c), "ddd") }
	f.confirmFn = func(string) *hookcore.CompliancePipelineResult {
		return &hookcore.CompliancePipelineResult{Decision: hookcore.Modify}
	}
	if err := Run(context.Background(), f, Config{TailWindowBytes: 3, MaxBufferBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	if len(f.confirmCalls) == 0 {
		t.Fatal("confirm did not run on the prescan hit — the gate was wrongly skipped after compaction (leak class)")
	}
	if last := f.confirmCalls[len(f.confirmCalls)-1]; strings.Contains(last, "aaa") {
		t.Fatalf("confirm saw the delivered+compacted prefix %q; want the held window only", last)
	}
	if !f.escalated {
		t.Fatal("expected escalation on the confirmed redact — detection must survive compaction")
	}
}

// TestWindowedConfirmExcludesDeliveredPrefix is the precise #12 windowing assertion the
// existing TestCompactionWindowedConfirm cannot make: with TailWindowBytes=3 and a prescan
// hit on the THIRD unit, exactly one unit ("aaa") has been delivered (deliveredScanLen=3)
// but scanBuf is NOT yet compacted (deliveredScanLen is not > the window), so the delivered
// prefix is still physically present in scanBuf at confirm time. The confirm MUST receive
// only the held window "bbbccc", never the full "aaabbbccc" — pinning Confirm to
// scanBuf[deliveredScanLen:]. This kills a "confirm the whole buffer" mutant that the
// compacting test (which rebases deliveredScanLen to 0, making windowed and full identical)
// lets survive.
func TestWindowedConfirmExcludesDeliveredPrefix(t *testing.T) {
	f := newFake([]fakeUnit{
		{id: 1, text: "aaa", content: 3, bytes: 3},
		{id: 2, text: "bbb", content: 3, bytes: 3},
		{id: 3, text: "ccc", content: 3, bytes: 3, done: true},
	})
	f.prescanFn = func(c []byte) bool { return strings.Contains(string(c), "ccc") }
	f.confirmFn = func(string) *hookcore.CompliancePipelineResult { return modify() }
	if err := Run(context.Background(), f, Config{TailWindowBytes: 3, MaxBufferBytes: 1 << 20, PrescanBatchBytes: 1}); err != nil {
		t.Fatal(err)
	}
	// Only "aaa" releases before the third-unit hit; scanBuf is still the un-compacted
	// "aaabbbccc" (deliveredScanLen=3, not > window=3) when the confirm fires.
	if !eq(f.delivered, []int{1}) {
		t.Fatalf("delivered = %v, want [1] (only the prefix released before the hit)", f.delivered)
	}
	if len(f.confirmCalls) != 1 {
		t.Fatalf("confirm ran %d times, want exactly 1 (on the third-unit hit)", len(f.confirmCalls))
	}
	if got := f.confirmCalls[0]; got != "bbbccc" {
		t.Fatalf("confirm saw %q, want the held window %q (delivered prefix \"aaa\" excluded; full buffer was \"aaabbbccc\")", got, "bbbccc")
	}
	if !f.escalated || !eq(f.escalateHeld, []int{2, 3}) {
		t.Fatalf("escalate held = %v (escalated=%v), want [2 3]", f.escalateHeld, f.escalated)
	}
}

// TestCompactionRebasePreservesPrescanGate pins the stale-high-confirmedLen leak class the
// existing compaction test lets survive: an early run of approve-confirms drives confirmedLen
// up to len(scanBuf), then a release past the window compacts scanBuf and REBASES confirmedLen
// down by the dropped length to a still-POSITIVE value (here 3, not clamped to 0). A later
// prescan hit on genuinely-new content ("ddd") must still satisfy the `len(scanBuf) >
// confirmedLen` gate and fire the confirm. A mutant that forgot to subtract the dropped length
// (leaving confirmedLen stale-high at 9) would fail the gate (6 > 9 is false), silently skip
// the confirm, and stream the fresh sensitive content unredacted.
func TestCompactionRebasePreservesPrescanGate(t *testing.T) {
	f := newFake([]fakeUnit{
		{id: 1, text: "aaa", content: 3, bytes: 3},
		{id: 2, text: "bbb", content: 3, bytes: 3},
		{id: 3, text: "ccc", content: 3, bytes: 3},
		{id: 4, text: "ddd", content: 3, bytes: 3, done: true},
	})
	f.prescanFn = func([]byte) bool { return true } // every new unit re-arms the gate
	f.confirmFn = func(c string) *hookcore.CompliancePipelineResult {
		if strings.Contains(c, "ddd") {
			return modify() // the fresh post-compaction content must reach this
		}
		return approve()
	}
	if err := Run(context.Background(), f, Config{TailWindowBytes: 3, MaxBufferBytes: 1 << 20, PrescanBatchBytes: 1}); err != nil {
		t.Fatal(err)
	}
	// aaa+bbb deliver before the final hit; compaction fires after bbb's release (drop=6,
	// confirmedLen rebased 9→3).
	if !eq(f.delivered, []int{1, 2}) {
		t.Fatalf("delivered = %v, want [1 2]", f.delivered)
	}
	last := f.confirmCalls[len(f.confirmCalls)-1]
	if !strings.Contains(last, "ddd") {
		t.Fatalf("final confirm = %q, want it to include the post-compaction content \"ddd\" (gate skipped on a stale-high confirmedLen?)", last)
	}
	if !f.escalated || !eq(f.escalateHeld, []int{3, 4}) {
		t.Fatalf("escalate held = %v (escalated=%v), want [3 4]", f.escalateHeld, f.escalated)
	}
}

// TestSustainedPrescanFP_ConfirmWorkIsLinear gates the #12 O(N²)→O(N) confirm fix with an
// asserting bound (the benchmark only prints). Under a perpetual prescan false-positive every
// unit fires a confirm; windowed confirm copies only the bounded tail (O(window) bytes per
// confirm → O(N) total), while the pre-#12 full-buffer confirm copied the whole accumulated
// prefix (O(N) bytes per confirm → O(N²) total). testing.AllocsPerRun is BLIND to this: the
// alloc COUNT is ~N either way — only the alloc BYTES diverge. So this measures TotalAlloc
// bytes at two stream lengths and asserts that 4× the units grows bytes ~linearly (< 8×); a
// quadratic-confirm regression would be ≈16× and fail.
func TestSustainedPrescanFP_ConfirmWorkIsLinear(t *testing.T) {
	run := func(n int) uint64 {
		units := make([]fakeUnit, n)
		for i := range units {
			units[i] = fakeUnit{id: i, text: "x", bytes: 1, content: 1, done: i == n-1}
		}
		f := newFake(units)
		f.prescanFn = func([]byte) bool { return true }
		f.confirmFn = func(string) *hookcore.CompliancePipelineResult { return approve() }
		var a, b runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&a)
		_ = Run(context.Background(), f, Config{TailWindowBytes: 8, MaxBufferBytes: 1 << 20})
		runtime.ReadMemStats(&b)
		return b.TotalAlloc - a.TotalAlloc
	}
	const small, large = 1000, 4000
	bSmall := run(small)
	bLarge := run(large)
	// 4× units → ~4× bytes under O(N). Generous 8× ceiling absorbs fixed per-unit overhead
	// (held / heldScanLens growth, fake bookkeeping); a quadratic confirm (~16×) blows it.
	if bLarge > bSmall*8 {
		t.Fatalf("confirm work grew %.1f× for %d× units (small=%d large=%d bytes); want ~linear (<8×), quadratic regression?",
			float64(bLarge)/float64(bSmall), large/small, bSmall, bLarge)
	}
}

// --- Batched prescan (#13 port) ---

// TestBatchedPrescan_CollapsesScanCount is the perf win: a long clean stream of small units
// triggers the cheap union prescan only once per PrescanBatchBytes of new content (plus the
// EOF flush), NOT once per unit — collapsing the cgo scan count. 300 ten-byte units = 3000
// scan bytes ⇒ ~2 batch scans + 1 EOF flush, far below 300 per-unit scans.
func TestBatchedPrescan_CollapsesScanCount(t *testing.T) {
	const units = 300
	us := make([]fakeUnit, units)
	for i := range us {
		us[i] = fakeUnit{id: i, text: strings.Repeat("x", 10), content: 10, bytes: 10, done: i == units-1}
	}
	f := newFake(us)
	f.prescanFn = func([]byte) bool { return false } // clean stream
	// Default PrescanBatchBytes (1024); huge window so nothing releases mid-stream.
	if err := Run(context.Background(), f, Config{TailWindowBytes: 1 << 20, MaxBufferBytes: 1 << 20}); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if got := len(f.prescanCalls); got > 5 {
		t.Fatalf("prescan ran %d times for %d units; batching should collapse it to ~3 (2 batch + 1 EOF)", got, units)
	}
	if len(f.delivered) != units {
		t.Fatalf("delivered %d units, want all %d", len(f.delivered), units)
	}
	if f.escalated {
		t.Fatal("clean stream must not escalate")
	}
}

// TestBatchedPrescan_EOFTailScannedNoLeak pins that a sub-threshold final content unit (one
// that never reaches the batch trigger) is still scanned at the EOF flush BEFORE the held
// tail is delivered raw — a confirmed hit there escalates rather than leaking.
func TestBatchedPrescan_EOFTailScannedNoLeak(t *testing.T) {
	f := newFake([]fakeUnit{
		{id: 1, text: "clean", content: 5, bytes: 5},
		{id: 2, text: "data", content: 4, bytes: 4},
		{id: 3, text: "secret", content: 6, bytes: 6, done: true}, // total 15B < batch threshold
	})
	f.prescanFn = func(b []byte) bool { return strings.Contains(string(b), "secret") }
	f.confirmFn = func(string) *hookcore.CompliancePipelineResult { return reject() }
	if err := Run(context.Background(), f, Config{TailWindowBytes: 1 << 20, MaxBufferBytes: 1 << 20}); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !f.escalated {
		t.Fatal("sub-threshold tail with a confirmed hit was NOT caught at the EOF flush — leak")
	}
	if !eq(f.escalateHeld, []int{1, 2, 3}) {
		t.Fatalf("escalate held = %v, want [1 2 3] (all held, nothing delivered)", f.escalateHeld)
	}
	if len(f.delivered) != 0 {
		t.Fatalf("delivered %v before the EOF-flush escalation, want none", f.delivered)
	}
	if got := len(f.prescanCalls); got != 1 {
		t.Fatalf("prescan ran %d times, want 1 (only the EOF flush; the batch trigger never fired)", got)
	}
}

// TestBatchedPrescan_MidStreamTriggerEscalates pins that once a unit pushes accumulated new
// content past PrescanBatchBytes, the batch trigger fires MID-stream (not only at EOF) and a
// confirmed hit escalates immediately with the still-held units.
func TestBatchedPrescan_MidStreamTriggerEscalates(t *testing.T) {
	f := newFake([]fakeUnit{
		{id: 1, text: "secret" + strings.Repeat("x", 1020), content: 1026, bytes: 1026}, // > 1024 ⇒ batch fires
		{id: 2, text: "later", content: 5, bytes: 5, done: true},                        // never reached
	})
	f.prescanFn = func(b []byte) bool { return strings.Contains(string(b), "secret") }
	f.confirmFn = func(string) *hookcore.CompliancePipelineResult { return reject() }
	if err := Run(context.Background(), f, Config{TailWindowBytes: 1 << 20, MaxBufferBytes: 1 << 20}); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !f.escalated || !eq(f.escalateHeld, []int{1}) {
		t.Fatalf("mid-stream batch trigger did not escalate at unit 1: escalated=%v held=%v", f.escalated, f.escalateHeld)
	}
	if len(f.prescanCalls) != 1 {
		t.Fatalf("prescan ran %d times, want 1 (the mid-stream batch trigger, before EOF)", len(f.prescanCalls))
	}
}

// TestBatchedPrescan_FlushBeforeDeliverNoLeak exercises the flush-before-deliver guard — the
// load-bearing soundness property when PrescanBatchBytes exceeds the tail window. With a small
// window and the default (large) batch threshold, the window fills and triggers a RELEASE
// before the batch trigger ever fires; the release must force a scanThrough over the held
// window first, so a pattern straddling the about-to-be-released prefix is caught (escalate),
// never delivered raw.
func TestBatchedPrescan_FlushBeforeDeliverNoLeak(t *testing.T) {
	f := newFake([]fakeUnit{
		{id: 1, text: "aaaaaaaa", content: 8, bytes: 8},
		{id: 2, text: "secretXX", content: 8, bytes: 8},
		{id: 3, text: "cccccccc", content: 8, bytes: 8, done: true}, // content 24 > window 20 ⇒ release
	})
	f.prescanFn = func(b []byte) bool { return strings.Contains(string(b), "secret") }
	f.confirmFn = func(string) *hookcore.CompliancePipelineResult { return reject() }
	// Window 20 < default batch threshold (1024): the batch trigger never fires; the
	// release-driven flush-before-deliver scan is the ONLY scan, and it must catch the hit.
	if err := Run(context.Background(), f, Config{TailWindowBytes: 20, MaxBufferBytes: 1 << 20}); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !f.escalated {
		t.Fatal("flush-before-deliver did NOT scan the held window before release — the secret would deliver raw")
	}
	if len(f.delivered) != 0 {
		t.Fatalf("delivered %v before the flush-before-deliver escalation, want none", f.delivered)
	}
	if len(f.prescanCalls) != 1 {
		t.Fatalf("prescan ran %d times, want 1 (the flush-before-deliver scan; batch threshold never reached)", len(f.prescanCalls))
	}
}

// TestBatchedPrescan_CoFiringBlockSoftAtEOF pins that a co-firing BlockSoft-carrying-redaction,
// confirmed only at the EOF flush under batching, still escalates on the enforcing ACTION
// (ActionFromDecision(BlockSoft)=block) rather than streaming the masked redact raw.
func TestBatchedPrescan_CoFiringBlockSoftAtEOF(t *testing.T) {
	f := newFake([]fakeUnit{{id: 1, text: "pii here", content: 8, bytes: 8, done: true}})
	f.prescanFn = func([]byte) bool { return true }
	f.confirmFn = func(string) *hookcore.CompliancePipelineResult {
		return &hookcore.CompliancePipelineResult{Decision: hookcore.BlockSoft}
	}
	if err := Run(context.Background(), f, Config{TailWindowBytes: 1 << 20, MaxBufferBytes: 1 << 20}); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !f.escalated || f.escalateRes == nil || f.escalateRes.Decision != hookcore.BlockSoft {
		t.Fatalf("co-firing BlockSoft at EOF flush did not escalate: escalated=%v res=%+v", f.escalated, f.escalateRes)
	}
	if len(f.delivered) != 0 {
		t.Fatalf("delivered %v before the co-firing escalation, want none", f.delivered)
	}
}

// TestBatchedPrescan_BoundarySpanningValue_LargeUnitNoLeak is the flush-before-deliver
// LOOKAHEAD regression (arch review): a LARGE content unit whose tail begins a sensitive
// value, followed by a small unit that completes it across the boundary. The large unit is
// batch-scanned ALONE (before its successor exists), so a flush guard keyed only on "front
// itself unscanned" (scannedLen < frontScanEnd) would release it raw — leaking the value's
// prefix — and then scan the suffix over a window that no longer holds the prefix, never
// matching. The MaxPatternBytes lookahead forces a re-scan over the completed value and
// escalates. (Defaults: W=8192, B=1024, MaxPatternBytes=4096.)
func TestBatchedPrescan_BoundarySpanningValue_LargeUnitNoLeak(t *testing.T) {
	u1text := strings.Repeat("a", 7196) + "secr" // 7200B, ends mid-"secret"
	u2text := "et" + strings.Repeat("b", 991)    // 993B ⇒ 7200+993 > 8192 ⇒ u1 released
	f := newFake([]fakeUnit{
		{id: 1, text: u1text, content: len(u1text), bytes: len(u1text)},
		{id: 2, text: u2text, content: len(u2text), bytes: len(u2text), done: true},
	})
	f.prescanFn = func(b []byte) bool { return strings.Contains(string(b), "secret") }
	f.confirmFn = func(string) *hookcore.CompliancePipelineResult { return reject() }
	if err := Run(context.Background(), f, Config{}); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !f.escalated {
		t.Fatal("boundary-spanning value across a large unit was NOT caught — the large unit leaked its prefix raw (flush lookahead missing/too small)")
	}
	if len(f.delivered) != 0 {
		t.Fatalf("delivered %v before the boundary escalation, want none (the large unit must not flush raw)", f.delivered)
	}
}

// BenchmarkRun_SustainedPrescanFP guards the O(N²)→O(N) confirm fix: a stream of N tiny
// content units under a perpetual prescan false-positive. Before #12 each unit ran a
// full-buffer confirm (Σ length = O(N²)); after, each confirm is windowed to O(window)
// and scanBuf is compacted, so total work + retained memory are O(N).
func BenchmarkRun_SustainedPrescanFP(b *testing.B) {
	units := make([]fakeUnit, 2000)
	for i := range units {
		units[i] = fakeUnit{id: i, text: "x", bytes: 1, content: 1, done: i == len(units)-1}
	}
	b.ReportAllocs()
	for range b.N {
		f := newFake(units)
		f.prescanFn = func([]byte) bool { return true }
		f.confirmFn = func(string) *hookcore.CompliancePipelineResult {
			return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
		}
		_ = Run(context.Background(), f, Config{TailWindowBytes: 8, MaxBufferBytes: 1 << 20})
	}
}
