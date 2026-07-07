// Package decision defines the core decision vocabulary for the compliance
// hook pipeline: Decision, its named constants (Approve, RejectHard, etc.),
// and the result types (CompliancePipelineResult, HookResult, ContentBlock,
// BlockingRule) that are shared across the pipeline, audit emitter, and
// every hook implementation.
//
// Types live here so that the pipeline/ and compliance/ packages can import
// them without creating an import cycle through the full hooks package tree.
package decision

import (
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Decision represents the outcome of a hook evaluation.
type Decision string

const (
	Approve    Decision = "APPROVE"
	RejectHard Decision = "REJECT_HARD"
	BlockSoft  Decision = "BLOCK_SOFT"
	// Modify indicates the transaction should be modified before forwarding.
	// Valid in the Hook interface; the Go compliance-proxy never binds MODIFY hooks.
	Modify  Decision = "MODIFY"
	Abstain Decision = "ABSTAIN"
)

// ContentBlock is a provider-agnostic content unit. Retained for hook
// implementations that still emit transitional ModifiedContent on HookResult;
// new consumers should use TransformSpans via normalize.ApplySpans instead.
type ContentBlock struct {
	Role string `json:"role"`           // "user", "assistant", "system", "tool"
	Type string `json:"type"`           // "text", "image", "tool_call", "tool_result"
	Text string `json:"text,omitempty"` // text content
	Raw  []byte `json:"raw,omitempty"`  // original JSON for non-text types
}

// BlockingRule is the attribution record for a rule-pack match that caused
// a hook to reject (hard or soft) a request. It is serialized to the
// traffic audit table so operators can trace a reject back to the exact
// pack/version/rule that fired.
type BlockingRule struct {
	Pack        string   `json:"pack"`
	PackVersion string   `json:"pack_version"`
	RuleID      string   `json:"rule_id"`
	Category    string   `json:"category,omitempty"`
	Severity    string   `json:"severity,omitempty"`
	Labels      []string `json:"labels,omitempty"`
}

// InflightAction is the policy applied to the upstream-bound copy of
// the body when a content-touching hook matches.
type InflightAction string

const (
	InflightApprove   InflightAction = "approve"
	InflightBlockHard InflightAction = "block-hard"
	InflightBlockSoft InflightAction = "block-soft"
	InflightRedact    InflightAction = "redact"
)

// StorageAction is the policy applied to the audit-log-bound copy
// (traffic_event_normalized.*_normalized JSON) when a content-touching
// hook matches.
type StorageAction string

const (
	StorageKeep        StorageAction = "keep"
	StorageRedact      StorageAction = "redact"
	StorageDropContent StorageAction = "drop-content"
)

// Action is the single hook match-outcome axis. It replaces the orthogonal
// InflightAction × StorageAction pair and governs both the inflight
// disposition and what is stored:
//   - approve: forward/return unchanged, store as-is
//   - redact:  rewrite the payload; the same masked body is forwarded,
//     returned, and stored
//   - block:   reject (403 on a proxy; connection drop on the agent) and
//     store the redacted copy
type Action string

const (
	ActionApprove Action = "approve"
	ActionRedact  Action = "redact"
	ActionBlock   Action = "block"
)

// Valid reports whether a is one of the three defined actions.
func (a Action) Valid() bool {
	switch a {
	case ActionApprove, ActionRedact, ActionBlock:
		return true
	}
	return false
}

// ActionFromLegacy maps the deprecated onMatch (inflightAction, storageAction)
// pair to the single Action. The new action follows the inflight axis; an
// approve paired with a redacting storage policy upgrades to redact (the
// compliance-safe direction, never less masked than before). An empty inflight
// is the legacy match default (block-hard).
func ActionFromLegacy(inflight InflightAction, storage StorageAction) Action {
	switch inflight {
	case InflightBlockHard, InflightBlockSoft:
		return ActionBlock
	case InflightRedact:
		return ActionRedact
	case InflightApprove:
		if storage == StorageRedact || storage == StorageDropContent {
			return ActionRedact
		}
		return ActionApprove
	}
	return ActionBlock
}

// CompliancePipelineResult is the aggregated result from the hook pipeline.
type CompliancePipelineResult struct {
	Decision    Decision
	Reason      string
	ReasonCode  string
	HookResults []HookResult
	Tags        []string `json:"tags,omitempty"` // union of tags emitted across all hooks
	// ModifiedContent is retained for callers that have not yet adopted
	// TransformSpan-based rewriting. New consumers use TransformSpans.
	ModifiedContent []ContentBlock `json:"modifiedContent,omitempty"`
	// TransformSpans is the union of byte-level modifications emitted by
	// every hook in this pipeline run.
	TransformSpans []normalize.TransformSpan `json:"transformSpans,omitempty"`
	// Action is the strictest single-axis outcome aggregated across the hooks
	// that matched this run. It drives both the inflight disposition and what
	// is stored (approve = as-is; redact/block = the redacted copy).
	Action Action `json:"action,omitempty"`
	// BlockingRule is the rule-pack attribution that caused the pipeline's
	// (reject) decision.
	BlockingRule *BlockingRule `json:"blockingRule,omitempty"`
	// RedactionApplicable reports whether an ENFORCING per-hook decision
	// (Modify or BlockSoft) contributed an APPLICABLE redaction artifact that
	// the aggregate actually carries — ModifiedContent (Modify hooks only; the
	// merge does not carry a BlockSoft hook's ModifiedContent) or a span whose
	// ContentAddress is not an audit-only sentinel. It is the single signal
	// CarriesRedaction() consults for the BlockSoft branch, separating a real
	// inflight redaction masked behind a co-firing soft-block from an advisory
	// approve-webhook / modify-webhook span set that cannot be applied. Derived
	// in pipeline.mergeResults where per-hook decisions are visible; it cannot
	// be reconstructed post-merge (the aggregate span union mixes advisory and
	// enforcing spans). json:"-" — it is an in-memory predicate input, not a
	// persisted/serialized field.
	RedactionApplicable bool `json:"-"`
}

// CarriesRedaction reports whether this result requires applying a redaction before
// delivery — a Modify, OR a BlockSoft that masks a co-firing redact (StrictestDecision
// promotes the aggregate Decision to BlockSoft while the redact's artifact is carried).
// Keying on Decision==Modify alone would silently DROP such a redaction and
// deliver/forward the flagged content raw.
//
// The BlockSoft branch gates on RedactionApplicable, NOT on raw span/content presence:
// a co-firing approve-webhook (or a modify-webhook reconciled under an approve ceiling)
// emits AUDIT-ONLY spans (ContentAddress = the audit-only sentinel, which ApplySpans
// never resolves), so keying on len(TransformSpans)>0 would report a redaction the
// aggregate cannot apply and over-block (fail-closed) a stream that should soft-deliver.
// RedactionApplicable is true only when an enforcing hook contributed an artifact the
// aggregate actually carries (see its field doc). A standalone soft-block with no
// applicable artifact carries no redaction and folds to block per the dispatch sites.
//
// Consumers MUST gate redaction on this predicate, not on Decision==Modify, on EVERY
// path (request + response, stream + non-stream) so a co-firing redact+soft-block masks
// and delivers rather than leaking or failing closed.
func (r *CompliancePipelineResult) CarriesRedaction() bool {
	if r == nil {
		return false
	}
	if r.Decision == Modify {
		return true
	}
	return r.Decision == BlockSoft && r.RedactionApplicable
}

// HookResult is the output produced by a single hook execution.
type HookResult struct {
	Order            int      `json:"order"` // execution order (0-based) within the pipeline
	HookID           string   `json:"hookId"`
	ImplementationID string   `json:"implementationId,omitempty"`
	HookName         string   `json:"hookName"`
	Decision         Decision `json:"decision"`
	Reason           string   `json:"reason,omitempty"`
	ReasonCode       string   `json:"reasonCode,omitempty"`
	// LatencyMs is the truncated integer-millisecond wall-clock for this hook,
	// kept for backward compatibility. It is the integer-ms floor of LatencyUs and
	// is NEVER clamped: per-hook latency is summed downstream, so clamping a
	// sub-millisecond hook to ≥1 would inflate the aggregate.
	LatencyMs int `json:"latencyMs"`
	// LatencyUs is the precise microsecond wall-clock for this hook. Hooks run at
	// microsecond scale, so LatencyMs truncates to 0 for sub-millisecond hooks;
	// LatencyUs carries the real value for the audit aggregates and the UI.
	LatencyUs int `json:"latencyUs"`
	// Tags emitted by this hook; merged into the pipeline-wide set.
	Tags            []string       `json:"tags,omitempty"`
	Error           string         `json:"error,omitempty"` // non-empty if the hook errored
	ModifiedContent []ContentBlock `json:"modifiedContent,omitempty"`
	// TransformSpans are the byte-level modifications this hook produced.
	TransformSpans []normalize.TransformSpan `json:"transformSpans,omitempty"`
	// Action reflects this hook's onMatch.action policy when the hook matched.
	Action Action `json:"action,omitempty"`
	// BlockingRule, when non-nil, identifies the rule-pack rule that
	// produced the (reject) Decision.
	BlockingRule *BlockingRule `json:"blockingRule,omitempty"`
}

// Standard ReasonCode constants used on HookResult.ReasonCode.
const (
	ReasonRedactInflightUnsupported = "REDACT_INFLIGHT_UNSUPPORTED"
	ReasonRedactStorageOnlyByPolicy = "REDACT_STORAGE_ONLY_BY_POLICY"
	ReasonStorageDroppedByPolicy    = "STORAGE_DROPPED_BY_POLICY"
	ReasonAIGuardSuggestedVsPolicy  = "AIGUARD_SUGGESTED_VS_POLICY"
	// ReasonFailClosed marks a request/response refused because a mandatory
	// (fail-closed) hook could not be built under a strict (appliance) policy,
	// so the traffic could not be inspected: the strict
	// caller refuses uninspectable traffic rather than forwarding it.
	ReasonFailClosed = "COMPLIANCE_FAIL_CLOSED"
)
