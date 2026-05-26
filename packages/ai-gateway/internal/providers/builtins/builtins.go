// Package provbuiltins wires every built-in AdapterSpec into a
// [provcore.Registry]. It lives in its own package to break the
// import cycle between [providers] (where the interfaces live) and
// the per-format subpackages [spec_openai, spec_anthropic, ...].
//
// Callers (typically cmd/ai-gateway) call [Register] once during
// startup and then invoke [provcore.Registry.Freeze].
package provbuiltins

import (
	"fmt"
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provdispatch "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/dispatch"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/azure"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/bedrock"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/cohere"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/deepseek"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/fireworks"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/glm"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/groq"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/huggingface"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/minimax"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/mistral"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/moonshot"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/voyage"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/perplexity"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/replicate"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/together"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/vertex"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/xai"
)

// Register installs every built-in adapter spec into reg. Panics on a
// duplicate registration or structurally invalid spec — both are
// programming errors that must abort startup.
//
// allowlist is the resolved forward-header allowlist (typically
// produced by forwardheader.Resolve from the operator's
// ai-gateway.dev.yaml). It is passed into every spec adapter at
// construction so the same read-only structure backs every adapter's
// header-filter decisions. Pass nil only in tests; production startup
// always supplies a resolved value.
func Register(reg *provcore.Registry, allowlist *forwardheader.Resolved, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	specs := []provcore.AdapterSpec{
		openai.NewSpec(log),
		deepseek.NewSpec(log),
		glm.NewSpec(log),
		azure.NewSpec(log),
		anthropic.NewSpec(log),
		gemini.NewSpec(log),
		minimax.NewSpec(log),
		bedrock.NewSpec(log),
		vertex.NewSpec(log),
		cohere.NewSpec(log),
		huggingface.NewSpec(log),
		replicate.NewSpec(log),
		mistral.NewSpec(log),
		xai.NewSpec(log),
		groq.NewSpec(log),
		perplexity.NewSpec(log),
		together.NewSpec(log),
		fireworks.NewSpec(log),
		moonshot.NewSpec(log),
		voyage.NewSpec(log),
	}
	registerSpecs(reg, specs, allowlist, log, provcore.AllFormats())
}

// registerSpecs is the testable seam for Register. It validates each spec,
// detects duplicates within the slice, and verifies the slice covers every
// format in wantFormats. Behaviour identical to the inlined loop in Register;
// extracted so unit tests can exercise the panic branches by passing
// hand-crafted specs slices.
func registerSpecs(
	reg *provcore.Registry,
	specs []provcore.AdapterSpec,
	allowlist *forwardheader.Resolved,
	log *slog.Logger,
	wantFormats []provcore.Format,
) {
	seen := make(map[provcore.Format]bool, len(specs))
	for _, s := range specs {
		if !s.Valid() {
			panic(fmt.Sprintf("provbuiltins: invalid spec for format %q", s.Format))
		}
		if seen[s.Format] {
			panic(fmt.Sprintf("provbuiltins: duplicate spec for format %q", s.Format))
		}
		seen[s.Format] = true
		reg.MustRegister(provdispatch.NewSpecAdapterWithAllowlist(s, allowlist, log))
	}
	for _, want := range wantFormats {
		if !seen[want] {
			panic(fmt.Sprintf("provbuiltins: missing format %q", want))
		}
	}
}
