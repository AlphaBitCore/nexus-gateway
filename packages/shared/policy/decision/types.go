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
	// StorageAction is the strictest operator policy declared across the
	// hooks that matched this run.
	StorageAction StorageAction `json:"storageAction,omitempty"`
	// BlockingRule is the rule-pack attribution that caused the pipeline's
	// (reject) decision.
	BlockingRule *BlockingRule `json:"blockingRule,omitempty"`
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
	LatencyMs        int      `json:"latencyMs"`
	// Tags emitted by this hook; merged into the pipeline-wide set.
	Tags  []string `json:"tags,omitempty"`
	Error string   `json:"error,omitempty"` // non-empty if the hook errored
	ModifiedContent []ContentBlock `json:"modifiedContent,omitempty"`
	// TransformSpans are the byte-level modifications this hook produced.
	TransformSpans []normalize.TransformSpan `json:"transformSpans,omitempty"`
	// StorageAction reflects this hook's onMatch.storageAction policy
	// when the hook matched.
	StorageAction StorageAction `json:"storageAction,omitempty"`
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
)
