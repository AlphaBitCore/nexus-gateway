package pipeline

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/redact"
)

// The silent data-loss bug this guards: decision.Action is a string, so a
// result built as &CompliancePipelineResult{Decision: Approve} — decision set,
// action not — carries Action == "". redact.StorageRawBody matches only
// approve/redact/block and returns nil otherwise, so that result DROPS the
// captured body and the row persists with a NULL body, uncounted and unlogged.
//
// It is not hypothetical: the compliance proxy builds exactly that result when
// no response hooks are bound, and consequently recorded a request body on
// every bumped row and a response body on NONE, while the gateway — which
// guards the same trap at each of its own call sites — recorded both.
func TestStageAction_EmptyActionApproves_SoTheBodyIsNotSilentlyDropped(t *testing.T) {
	noHooksRan := &CompliancePipelineResult{Decision: core.Approve} // Action deliberately unset

	// stageAction derives the action from the DECISION when the producer left it
	// unset. For this result — decision Approve, no hooks ran — that is approve,
	// which is the same outcome the empty action used to reach through the gate.
	// The derivation exists for the decisions where the two differ: a hand-built
	// RejectHard with no action must be governed as a block, not as "nothing asked
	// for redaction" (see TestStageAction_DerivesTheActionFromTheDecision).
	if got := stageAction(noHooksRan); got != decision.ActionApprove {
		t.Fatalf("stageAction = %q for an approve result with no action set; want %q",
			got, decision.ActionApprove)
	}

	// The observable that actually matters, and the reason the rule moved: the
	// body survives the gate every service shares.
	body := []byte(`{"choices":[{"message":{"content":"kept"}}]}`)
	got, ok := redact.StorageRawBodyChecked(body, nil, stageAction(noHooksRan))
	if !ok {
		t.Error("the gate reported an unrecognised action for an unset one; empty must mean approve")
	}
	if len(got) != len(body) {
		t.Fatalf("captured body of %d bytes persisted as %d bytes — a hookless response is being dropped",
			len(body), len(got))
	}
}

// The emitter's own wrapper must both apply the gate and SAY something when the
// action cannot be named. A silent nil body is the shape this whole finding took:
// the row persists with a NULL body and nothing counts or logs it.
func TestGateStorageBody_UnknownActionIsLoggedNotSilent(t *testing.T) {
	var buf bytes.Buffer
	e := NewAuditEmitter(nil, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	body := []byte("captured bytes that must not be persisted under an unnamed policy")

	if got := e.gateStorageBody(body, nil, decision.Action("mangled"), "response"); got != nil {
		t.Errorf("an unrecognised action persisted %d bytes; want nothing stored", len(got))
	}
	out := buf.String()
	if !strings.Contains(out, "unrecognised match action") || !strings.Contains(out, "mangled") {
		t.Errorf("want a WARN naming the action that was not understood, got:\n%s", out)
	}
	if !strings.Contains(out, "stage=response") {
		t.Errorf("want the stage in the WARN so the report says which body vanished, got:\n%s", out)
	}

	// The reverse assertion: a recognised action must NOT log, or the line becomes
	// noise on every request and stops being read.
	buf.Reset()
	if got := e.gateStorageBody(body, nil, decision.ActionApprove, "request"); len(got) != len(body) {
		t.Errorf("approve persisted %d of %d bytes", len(got), len(body))
	}
	if buf.Len() != 0 {
		t.Errorf("a recognised action must log nothing, got:\n%s", buf.String())
	}
}

// The mirror: a real redact decision must still replace the raw body, so the
// empty-action mapping cannot become a blanket "always store the raw bytes".
func TestStageAction_RedactStillReplacesTheRawBody(t *testing.T) {
	redacted := &CompliancePipelineResult{Decision: core.Approve, Action: decision.ActionRedact}
	raw, masked := []byte("card 4111111111111111"), []byte("card ****")

	got := redact.StorageRawBody(raw, masked, stageAction(redacted))
	if string(got) != string(masked) {
		t.Fatalf("under redact the persisted body is %q; want the masked copy %q — raw sensitive bytes must never reach the store", got, masked)
	}
}

// A result that reports a DECISION with no action must be governed by that
// decision, not treated as "nothing asked for redaction".
//
// The case that matters is the fail-closed refusal the compliance proxy emits
// when the request pipeline cannot be BUILT: `{Decision: RejectHard}` with no
// action. Reading that as an empty action made the gate persist the captured raw
// body of the ONE request class the product knows it could not scan — strictly
// more permissive than an ordinary scanned block, which persists nothing. Found
// by adversarial review of the empty-action rule and reproduced before the fix.
func TestStageAction_DerivesTheActionFromTheDecision(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"user","content":"my ssn is 123-45-6789"}]}`)

	cases := []struct {
		name       string
		result     *CompliancePipelineResult
		wantAction decision.Action
		wantStored int
	}{
		{
			"unbuildable fail-closed pipeline refuses: governed as a block, stores nothing",
			&CompliancePipelineResult{Decision: core.RejectHard}, // the real literal — no action
			decision.ActionBlock, 0,
		},
		{
			"soft block with no action is still a block",
			&CompliancePipelineResult{Decision: core.BlockSoft},
			decision.ActionBlock, 0,
		},
		{
			"a modify with no action must not store the raw copy",
			&CompliancePipelineResult{Decision: core.Modify},
			decision.ActionRedact, 0,
		},
		{
			"approve with no action stores the captured bytes",
			&CompliancePipelineResult{Decision: core.Approve},
			decision.ActionApprove, len(raw),
		},
		{
			"an explicit action always wins over the derivation",
			&CompliancePipelineResult{Decision: core.RejectHard, Action: decision.ActionApprove},
			decision.ActionApprove, len(raw),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stageAction(tc.result); got != tc.wantAction {
				t.Errorf("stageAction = %q, want %q", got, tc.wantAction)
			}
			got, _ := redact.StorageRawBodyChecked(raw, nil, stageAction(tc.result))
			if len(got) != tc.wantStored {
				t.Errorf("persisted %d bytes, want %d — a refusal the product could not scan must not be "+
					"more permissive than one it could", len(got), tc.wantStored)
			}
		})
	}

	// A nil result still means "this stage never ran", which is not a decision at
	// all and must keep storing the captured bytes.
	if got := stageAction(nil); got != "" {
		t.Errorf("stageAction(nil) = %q, want the empty action", got)
	}
}
