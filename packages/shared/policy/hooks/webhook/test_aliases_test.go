// Test-only aliases from core so test files can use bare names.
package webhook

import "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"

type (
	Hook       = core.Hook
	HookConfig = core.HookConfig
	HookInput  = core.HookInput
	HookResult = core.HookResult
	Decision   = core.Decision
)

const (
	Approve    = core.Approve
	RejectHard = core.RejectHard
	BlockSoft  = core.BlockSoft
	Modify     = core.Modify
	Abstain    = core.Abstain
)

var PayloadFromTextSegments = core.PayloadFromTextSegments
