//go:build vectorscan

package validators

import "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"

// defaultCompiler selects the Vectorscan (cgo) matcher when the binary is built
// with `-tags vectorscan`. It returns the hybrid Vectorscan+RE2-residual
// compiler, so every rule still has a working path (no coverage loss). See
// rulepack_compiler_re2.go for the default build's selection.
func defaultCompiler() matcherCompiler { return matcher.CompileVectorscan }
