// Test-only aliases from core so test files can use bare names.
package access

import "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"

type (
	Hook       = core.Hook
	HookConfig = core.HookConfig
	HookInput  = core.HookInput
	HookResult = core.HookResult
)

const (
	Approve    = core.Approve
	RejectHard = core.RejectHard
)
