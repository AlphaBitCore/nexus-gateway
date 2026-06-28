//go:build !vectorscan

package validators

import "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"

// defaultCompiler selects the rule-pack matcher engine for the default build:
// the pure-Go RE2 matcher. A build tagged `vectorscan` swaps in the Vectorscan
// (cgo) compiler via rulepack_compiler_vectorscan.go. NewRulePackEngine routes
// through this so the engine choice is a build-time decision, not a runtime one.
func defaultCompiler() matcherCompiler { return matcher.CompileRE2 }
