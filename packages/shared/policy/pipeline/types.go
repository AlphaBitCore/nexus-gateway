package pipeline

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// Type aliases exported from this package for callers that import pipeline
// rather than hooks/core directly.

type (
	Decision                 = core.Decision
	CompliancePipelineResult = core.CompliancePipelineResult
	HookResult               = core.HookResult
	HookConfig               = core.HookConfig
	Hook                     = core.Hook
	HookFactory              = core.HookFactory
	HookInput                = core.HookInput
	ContentBlock             = core.ContentBlock
)

const (
	Approve    = core.Approve
	RejectHard = core.RejectHard
	BlockSoft  = core.BlockSoft
	Modify     = core.Modify
	Abstain    = core.Abstain
)

var PayloadFromTextSegments = core.PayloadFromTextSegments
