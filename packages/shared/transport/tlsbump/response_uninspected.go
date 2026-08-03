package tlsbump

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// uninspectedResponse is the response-stage result for a bumped flow that no
// response hook examined.
//
// Every relay path that reaches the audit emitter must hand it a response-stage
// result, and six of them had nothing to hand it: no response pipeline was bound
// (the deployment enables no response hooks), the body was unreadable, the flow
// took the non-AI fast path, the pipeline failed to build and fail-open let it
// through, the SSE stream ran no response stage, or the upstream failed before a
// response existed at all. Each of those constructed
// `CompliancePipelineResult{Decision: Approve}` — so `response_hook_decision`
// read APPROVE for traffic nothing had looked at.
//
// That is not a cosmetic difference. Observed live on 2026-07-28: with every
// HookConfig row disabled, bumped traffic wrote rows carrying
// `response_hook_decision = APPROVE` with `response_hooks_pipeline` NULL. With
// the same build and the hooks enabled, the same column read APPROVE with a
// populated pipeline. One column value, two opposite meanings — "inspected and
// cleared" and "never looked at" — and no way for a consumer to tell them apart.
// A compliance owner filtering for APPROVE counts the second as the first.
//
// The request stage already had this right: it passes its result through
// unchanged, so an absent request pipeline lands as NULL. This makes the
// response stage agree, rather than inventing a third spelling. sse_audit.go
// documented the intended contract in a comment — "default decision stays empty
// rather than fabricating an Approve" — directly above the line that fabricated
// one; this is that comment's code.
//
// Decision is deliberately left empty: the audit emitter's stagePayload writes
// `string(r.Decision)` into the column, and an empty decision becomes NULL.
// Action is set explicitly because it feeds a different consumer — the emitter's
// storage gate reads stageAction to decide whether the captured body may be
// persisted verbatim. Leaving the whole result nil would satisfy the column but
// strip that gate of a nameable action, changing body-capture behaviour on every
// deployment that runs no response hooks. Honesty in the audit column must not
// cost a change in what gets stored.
func uninspectedResponse() *core.CompliancePipelineResult {
	return &core.CompliancePipelineResult{Action: decision.ActionApprove}
}
