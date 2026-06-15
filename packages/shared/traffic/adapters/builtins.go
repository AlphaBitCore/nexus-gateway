// Package adapters provides built-in adapter registrations for the traffic
// interception framework.
package adapters

import (
	"fmt"
	"slices"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/anthropic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/azure"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/bedrock"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/cohere"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/deepseek"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/fireworks"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/gemini"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/glm"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/groq"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/huggingface"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/minimax"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/mistral"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/moonshot"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/perplexity"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/replicate"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/together"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/vertex"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/voyage"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/xai"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/generic/generic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/ide/codeium"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/ide/continuedev"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/ide/cursor"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/ide/githubcopilot"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/ide/replitai"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/ide/tabnine"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/anthropicconsoleweb"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/boltweb"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/characterweb"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/chatglmweb"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/chatgptweb"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/claudeweb"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/copilotmsweb"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/deepseekweb"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/devinweb"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/geminiweb"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/githubcopilotweb"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/googleaistudioweb"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/grokweb"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/huggingchatweb"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/kimiweb"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/m365copilotweb"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/mistralweb"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/openaiplatformweb"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/perplexityweb"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/poeweb"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/v0web"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/youweb"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// builtinEntries is the single source of truth for built-in adapter IDs and
// factories. RegisterBuiltins and BuiltinTrafficAdapterIDs both derive from
// this table so the admin traffic-adapter catalog cannot drift from runtime
// registration.
var builtinEntries = []struct {
	id      string
	factory traffic.AdapterFactory
}{
	{"openai-compat", func() traffic.Adapter { return &openai.Adapter{} }},
	{"generic-jsonpath", func() traffic.Adapter { return &generic.Adapter{} }},
	{"anthropic", func() traffic.Adapter { return &anthropic.Adapter{} }},
	{"gemini", func() traffic.Adapter { return &gemini.Adapter{} }},
	{"minimax", func() traffic.Adapter { return &minimax.Adapter{} }},
	{"azure-openai", func() traffic.Adapter { return &azure.Adapter{} }},
	{"bedrock", func() traffic.Adapter { return &bedrock.Adapter{} }},
	{"vertex", func() traffic.Adapter { return &vertex.Adapter{} }},
	{"glm", func() traffic.Adapter { return &glm.Adapter{} }},
	{"deepseek", func() traffic.Adapter { return &deepseek.Adapter{} }},
	{"chatgpt-web", func() traffic.Adapter { return &chatgptweb.Adapter{} }},
	{"claude-web", func() traffic.Adapter { return &claudeweb.Adapter{} }},
	{"gemini-web", func() traffic.Adapter { return &geminiweb.Adapter{} }},
	{"copilot-ms-web", func() traffic.Adapter { return &copilotmsweb.Adapter{} }},
	{"github-copilot", func() traffic.Adapter { return &githubcopilot.Adapter{} }},
	{"cursor", func() traffic.Adapter { return &cursor.Adapter{} }},
	{"codeium", func() traffic.Adapter { return &codeium.Adapter{} }},
	{"grok-web", func() traffic.Adapter { return &grokweb.Adapter{} }},
	{"perplexity-web", func() traffic.Adapter { return &perplexityweb.Adapter{} }},
	{"mistral-web", func() traffic.Adapter { return &mistralweb.Adapter{} }},
	{"openai-platform-web", func() traffic.Adapter { return &openaiplatformweb.Adapter{} }},
	{"anthropic-console-web", func() traffic.Adapter { return &anthropicconsoleweb.Adapter{} }},
	{"google-aistudio-web", func() traffic.Adapter { return &googleaistudioweb.Adapter{} }},
	{"tabnine", func() traffic.Adapter { return &tabnine.Adapter{} }},
	{"m365-copilot-web", func() traffic.Adapter { return &m365copilotweb.Adapter{} }},
	{"together", func() traffic.Adapter { return &together.Adapter{} }},
	{"fireworks", func() traffic.Adapter { return &fireworks.Adapter{} }},
	{"deepseek-web", func() traffic.Adapter { return &deepseekweb.Adapter{} }},
	{"moonshot", func() traffic.Adapter { return &moonshot.Adapter{} }},
	{"kimi-web", func() traffic.Adapter { return &kimiweb.Adapter{} }},
	{"chatglm-web", func() traffic.Adapter { return &chatglmweb.Adapter{} }},
	{"cohere", func() traffic.Adapter { return &cohere.Adapter{} }},
	{"huggingface", func() traffic.Adapter { return &huggingface.Adapter{} }},
	{"continue-dev", func() traffic.Adapter { return &continuedev.Adapter{} }},
	{"v0-web", func() traffic.Adapter { return &v0web.Adapter{} }},
	{"bolt-web", func() traffic.Adapter { return &boltweb.Adapter{} }},
	{"replicate", func() traffic.Adapter { return &replicate.Adapter{} }},
	{"character-web", func() traffic.Adapter { return &characterweb.Adapter{} }},
	{"you-web", func() traffic.Adapter { return &youweb.Adapter{} }},
	{"huggingchat-web", func() traffic.Adapter { return &huggingchatweb.Adapter{} }},
	{"replit-ai", func() traffic.Adapter { return &replitai.Adapter{} }},
	{"devin-web", func() traffic.Adapter { return &devinweb.Adapter{} }},
	{"mistral", func() traffic.Adapter { return &mistral.Adapter{} }},
	{"xai", func() traffic.Adapter { return &xai.Adapter{} }},
	{"groq", func() traffic.Adapter { return &groq.Adapter{} }},
	{"perplexity", func() traffic.Adapter { return &perplexity.Adapter{} }},
	{"poe-web", func() traffic.Adapter { return &poeweb.Adapter{} }},
	{"github-copilot-web", func() traffic.Adapter { return &githubcopilotweb.Adapter{} }},
	{"voyage", func() traffic.Adapter { return &voyage.Adapter{} }},
}

// BuiltinTrafficAdapterIDs returns sorted canonical adapter IDs registered by
// RegisterBuiltins (for admin API / UI catalog).
func BuiltinTrafficAdapterIDs() []string {
	ids := make([]string, len(builtinEntries))
	for i := range builtinEntries {
		ids[i] = builtinEntries[i].id
	}
	slices.Sort(ids)
	return ids
}

// RegisterBuiltins registers all built-in adapters with the given registry.
// Called once at startup by each data plane service. Fatal on error because
// a failed registration at startup is a programming error.
func RegisterBuiltins(registry *traffic.AdapterRegistry) {
	for _, e := range builtinEntries {
		must(registry.Register(e.id, e.factory))
	}
}

// RegisterTier1AdapterNormalizers wires the subset of built-in traffic
// adapters that implement normalize.Normalizer into the given Hub-side
// normalize.Registry under their adapter IDs.
//
// traffic.Adapter is the per-host capability bundle used at runtime by
// compliance-proxy / agent (extract content for hooks, rewrite for redact,
// detect provider/model). normalize.Normalizer is the audit-time wire-format
// parser consulted by the Hub-side Coordinator (Registry.Normalize) when a
// captured audit envelope carries an adapter_type matching the registered ID.
// Adapters implementing both interfaces get Tier 1 (per-host confirmed parse,
// higher confidence) rather than Tier 2 (pattern probe, generic confidence).
//
// Adapters that do not implement normalize.Normalizer fall through to the
// Tier 2 PatternNormalizer wired by extract.WireTier2. Migrating an adapter
// to Tier 1 is a one-method change; the registration loop here picks it up
// automatically via type-assert.
//
// The standard-API vendor adapters (api/*) carry NO Normalize method:
// their wire formats are decoded by the shared codecs that
// normalize.RegisterDefaultAIBuiltins registers under the same
// adapter-type keys ("anthropic", "openai-compat", "gemini", …). Adding
// a Normalize method to one of those adapters would make this loop
// re-register its key and panic Hub startup with "duplicate
// registration" — that panic IS the lock-step guard: per-host
// Normalizers exist only for consumer/IDE surfaces whose IDs are
// disjoint from the codec key set.
func RegisterTier1AdapterNormalizers(reg *normalize.Registry) {
	seen := map[string]bool{}
	for _, e := range builtinEntries {
		// Build one instance to type-assert. Adapter instances are
		// stateless after Configure so this is safe to retain as the
		// live Normalizer; no per-domain Configure call is necessary
		// because Normalize doesn't consult per-domain config — it
		// parses by wire shape alone.
		inst := e.factory()
		if n, ok := inst.(normalize.Normalizer); ok {
			if seen[e.id] {
				continue
			}
			seen[e.id] = true
			reg.Register(e.id, n)
		}
	}
}

func must(err error) {
	if err != nil {
		panic(fmt.Errorf("adapter registration failed: %w", err))
	}
}
