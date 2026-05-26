// Test-only aliases from core so test files can use bare names.
package ratelimit

import "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"

type (
	HookConfig = core.HookConfig
	HookInput  = core.HookInput
	HookResult = core.HookResult
)

const (
	Approve    = core.Approve
	RejectHard = core.RejectHard
)

// toInt64 delegates to core.ToInt64 so existing test code compiles unchanged.
func toInt64(v any) (int64, bool) { return core.ToInt64(v) }
