package vendorbill

import (
	"sort"
	"testing"
)

func TestRegistry_ResolvesConfiguredVendors(t *testing.T) {
	r := NewRegistry(Config{OpenAIAdminKey: "sk-a", AnthropicAdminKey: "sk-b"})
	if r.Resolve("openai") == nil {
		t.Error("openai must resolve when its admin key is set")
	}
	if r.Resolve("anthropic") == nil {
		t.Error("anthropic must resolve when its admin key is set")
	}
	if got := r.Resolve("openai").ProviderKey(); got != "openai" {
		t.Errorf("resolved wrong source: %q", got)
	}
}

func TestRegistry_UnknownProviderNil(t *testing.T) {
	r := NewRegistry(Config{OpenAIAdminKey: "sk-a", AnthropicAdminKey: "sk-b"})
	for _, p := range []string{"gemini", "deepseek", "moonshot", ""} {
		if r.Resolve(p) != nil {
			t.Errorf("provider %q has no cost API in v1 and must resolve nil", p)
		}
	}
}

// The scope pins are the difference between a comparable row and an org_only
// one, so a config field that silently fails to reach its source would quietly
// make every row useless. Assert they land on the constructed sources.
func TestRegistry_ScopePinsReachTheSources(t *testing.T) {
	r := NewRegistry(Config{
		OpenAIAdminKey:       "sk-a",
		OpenAIAPIKeyID:       "key_gw123",
		AnthropicAdminKey:    "sk-b",
		AnthropicWorkspaceID: "wrkspc_gw",
	})
	oa, ok := r.Resolve("openai").(*openaiBillSource)
	if !ok {
		t.Fatal("openai source has unexpected type")
	}
	if oa.apiKeyID != "key_gw123" {
		t.Errorf("OpenAIAPIKeyID did not reach the source: %q", oa.apiKeyID)
	}
	an, ok := r.Resolve("anthropic").(*anthropicBillSource)
	if !ok {
		t.Fatal("anthropic source has unexpected type")
	}
	if an.workspaceID != "wrkspc_gw" {
		t.Errorf("AnthropicWorkspaceID did not reach the source: %q", an.workspaceID)
	}
}

func TestRegistry_ScopePinsOptional(t *testing.T) {
	r := NewRegistry(Config{OpenAIAdminKey: "sk-a", AnthropicAdminKey: "sk-b"})
	if oa := r.Resolve("openai").(*openaiBillSource); oa.apiKeyID != "" {
		t.Errorf("unset pin must stay empty, got %q", oa.apiKeyID)
	}
	if an := r.Resolve("anthropic").(*anthropicBillSource); an.workspaceID != "" {
		t.Errorf("unset pin must stay empty, got %q", an.workspaceID)
	}
}

func TestRegistry_EmptyKeyNotConfigured(t *testing.T) {
	// Anthropic key missing → covered-but-not-configured → nil, and openai still works.
	r := NewRegistry(Config{OpenAIAdminKey: "sk-a"})
	if r.Resolve("anthropic") != nil {
		t.Error("anthropic without an admin key must be unconfigured (nil)")
	}
	if r.Resolve("openai") == nil {
		t.Error("openai must still resolve")
	}
	keys := r.ConfiguredKeys()
	sort.Strings(keys)
	if len(keys) != 1 || keys[0] != "openai" {
		t.Errorf("ConfiguredKeys = %v, want [openai]", keys)
	}
}
