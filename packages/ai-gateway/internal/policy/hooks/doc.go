// Package hooks here is a contract-test mount point. It contains no
// production code — the directory exists so the three-end contract
// suite from packages/shared/policy/hooks/contract/ runs in the AI Gateway
// test binary (alongside the same suite running in CP + Agent test
// binaries).
//
// Real hook implementations live in packages/shared/policy/hooks/ and are
// wired into the AI Gateway runtime in packages/ai-gateway/cmd/
// ai-gateway/main.go (search for `goHooks.HookConfigRow` and
// `goHooks.BuildHookConfig`). When you need to add a new hook type,
// add it under packages/shared/policy/hooks/; this directory will pick it up
// automatically via the shared contract suite.
//
// Cross-ref: hook-architecture.md (Tier 1).
package hooks
