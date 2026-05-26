// Package compliance is a thin facade that re-exports shared types so existing
// compliance-proxy code continues to compile without widespread import rewrites.
// Keeping this indirection lets internal packages reference a single local
// import path; a future cleanup pass may inline the shared imports directly.
package compliance

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
)

// Re-exports from shared/hooks (types + constants).

type Decision = core.Decision

const (
	Approve    = core.Approve
	RejectHard = core.RejectHard
	BlockSoft  = core.BlockSoft
	Modify     = core.Modify
	Abstain    = core.Abstain
)

type CompliancePipelineResult = core.CompliancePipelineResult
type HookResult = core.HookResult
type HookConfig = core.HookConfig
type Hook = core.Hook
type HookFactory = core.HookFactory

// HookInput and ContentBlock re-exports (used by pipeline Execute).

type HookInput = core.HookInput
type ContentBlock = core.ContentBlock

// PayloadFromTextSegments builds a synthetic NormalizedPayload for extractor-based call sites.
var PayloadFromTextSegments = core.PayloadFromTextSegments

// Re-exports from shared/compliance (pipeline, policy).

type PolicyResolver = pipeline.PolicyResolver

var NewPolicyResolver = pipeline.NewPolicyResolver

type Pipeline = pipeline.Pipeline

var NewPipeline = pipeline.NewPipeline

// AuditEmitter re-exports (lives in shared/compliance/pipeline).

type AuditEmitter = pipeline.AuditEmitter
type AuditInfo = pipeline.AuditInfo

var NewAuditEmitter = pipeline.NewAuditEmitter
