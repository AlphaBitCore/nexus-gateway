package compliance

import "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"

// Re-export compliance pipeline metrics from shared so existing compliance-proxy
// code that references compliance.PipelineDuration etc. continues to compile.
var (
	PipelineDuration      = pipeline.PipelineDuration
	HookDuration          = pipeline.HookDuration
	HookDecisionTotal     = pipeline.HookDecisionTotal
	PipelineDecisionTotal = pipeline.PipelineDecisionTotal
	HookErrorTotal        = pipeline.HookErrorTotal
	HookTimeoutTotal      = pipeline.HookTimeoutTotal
)
