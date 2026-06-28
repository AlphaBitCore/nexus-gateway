// Test-only aliases from core so test files can use bare names.
package validators

import "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"

type (
	Hook         = core.Hook
	HookConfig   = core.HookConfig
	HookInput    = core.HookInput
	HookResult   = core.HookResult
	ContentBlock = core.ContentBlock
	Decision     = core.Decision
)

const (
	Approve    = core.Approve
	RejectHard = core.RejectHard
	BlockSoft  = core.BlockSoft
	Modify     = core.Modify

	ActionApprove = core.ActionApprove
	ActionRedact  = core.ActionRedact
	ActionBlock   = core.ActionBlock
)

var PayloadFromTextSegments = core.PayloadFromTextSegments

// Registry is a test-local registry containing the validators sub-package
// factories. Used by tests that verify factory dispatch via impl ID.
// Does NOT include access/ratelimit/webhook factories (not needed here).
var Registry = func() *core.HookRegistry {
	r := core.NewHookRegistry()
	r.Register("keyword-filter", NewKeywordFilter)
	r.Register("pii-detector", NewPiiDetector)
	r.Register("content-safety", NewContentSafety)
	r.Register("request-size-validator", NewRequestSizeValidator)
	r.Register("rulepack-engine", NewRulePackEngine)
	r.Register("quality-checker", NewQualityChecker)
	r.Freeze()
	return r
}()
