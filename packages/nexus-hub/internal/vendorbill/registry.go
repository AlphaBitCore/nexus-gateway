package vendorbill

import "net/http"

// Config carries the per-vendor admin keys (env-sourced, never yaml), the
// optional scope pins, plus optional base-URL overrides (tests / self-hosted
// gateways). A vendor with an empty admin key is omitted from the registry:
// that provider is "not configured", distinct from "not covered" (no
// VendorBillSource exists at all).
//
// The scope pins are what make reconciliation meaningful on a shared account.
// An admin key reports the whole organization, so without a pin the vendor
// total covers every project/workspace and every other API key in the account —
// diffing it against one gateway's spend compares two different things and the
// row is marked org_only (reference only, never alerted). Pinning costs one env
// var and avoids the alternative, which is restructuring a live vendor account
// so the gateway sits alone in its own project/workspace.
type Config struct {
	OpenAIAdminKey string
	// OpenAIAPIKeyID narrows the OpenAI bill to a single API key via the
	// endpoint's api_key_ids filter (e.g. "key_abc123") — the gateway's own key.
	OpenAIAPIKeyID string
	OpenAIBaseURL  string

	AnthropicAdminKey string
	// AnthropicWorkspaceID narrows the Anthropic bill to one workspace (e.g.
	// "wrkspc_abc123"). Anthropic exposes no per-key cost, so a workspace
	// dedicated to the gateway is the tightest attribution available.
	AnthropicWorkspaceID string
	AnthropicBaseURL     string

	HTTPClient *http.Client
}

// Registry resolves a Provider.adapterType to its configured VendorBillSource.
type Registry struct {
	sources map[string]VendorBillSource
}

// NewRegistry builds the set of configured sources from cfg. Only vendors with
// a non-empty admin key are registered.
func NewRegistry(cfg Config) *Registry {
	sources := map[string]VendorBillSource{}
	if cfg.OpenAIAdminKey != "" {
		sources[openaiProviderKey] = newOpenAIBillSource(cfg.OpenAIAdminKey, cfg.OpenAIAPIKeyID, cfg.OpenAIBaseURL, cfg.HTTPClient)
	}
	if cfg.AnthropicAdminKey != "" {
		sources[anthropicProviderKey] = newAnthropicBillSource(cfg.AnthropicAdminKey, cfg.AnthropicWorkspaceID, cfg.AnthropicBaseURL, cfg.HTTPClient)
	}
	return &Registry{sources: sources}
}

// Resolve returns the source for a provider adapter type, or nil when the
// provider has no configured source (either not covered in v1, or covered but
// missing its admin key).
func (r *Registry) Resolve(adapterType string) VendorBillSource {
	return r.sources[adapterType]
}

// ConfiguredKeys returns the provider adapter types that have a live source,
// so callers can iterate only the reconcilable providers.
func (r *Registry) ConfiguredKeys() []string {
	keys := make([]string, 0, len(r.sources))
	for k := range r.sources {
		keys = append(keys, k)
	}
	return keys
}
