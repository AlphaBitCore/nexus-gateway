package proxy

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

func TestIsSchemaCompatibleLegacy(t *testing.T) {
	cases := []struct {
		ingress  provcore.Format
		provider provcore.Format
		want     bool
	}{
		{provcore.FormatOpenAI, provcore.FormatOpenAI, true},
		{provcore.FormatOpenAI, provcore.FormatAnthropic, true},
		{provcore.FormatOpenAI, provcore.FormatGemini, true},
		{provcore.FormatAnthropic, provcore.FormatAnthropic, true},
		{provcore.FormatAnthropic, provcore.FormatOpenAI, false},
		{provcore.FormatGemini, provcore.FormatOpenAI, false},
		{provcore.FormatGLM, provcore.FormatGLM, true},
		{provcore.FormatGLM, provcore.FormatAnthropic, false},
	}
	for _, c := range cases {
		if got := isSchemaCompatibleLegacy(c.ingress, c.provider); got != c.want {
			t.Errorf("isSchemaCompatibleLegacy(%s,%s) = %v, want %v", c.ingress, c.provider, got, c.want)
		}
	}
}

func TestSchemaMode_LegacyNilBridge(t *testing.T) {
	ep := typology.WireShapeOpenAIChat
	cases := []struct {
		ingress  provcore.Format
		provider provcore.Format
		want     string
	}{
		{provcore.FormatOpenAI, provcore.FormatOpenAI, "passthrough"},
		{provcore.FormatOpenAI, provcore.FormatAnthropic, "translated"},
		{provcore.FormatAnthropic, provcore.FormatAnthropic, "passthrough"},
		{provcore.FormatAnthropic, provcore.FormatOpenAI, "rejected"},
		{provcore.FormatGLM, provcore.FormatGemini, "rejected"},
	}
	for _, c := range cases {
		if got := schemaMode(c.ingress, c.provider, ep, nil); got != c.want {
			t.Errorf("schemaMode(%s,%s,nil) = %q, want %q", c.ingress, c.provider, got, c.want)
		}
	}
}

func TestSchemaMode_WithHubBridge(t *testing.T) {
	bridge := canonicalbridge.New(provbuiltins.SchemaCodecs(nil))
	ep := typology.WireShapeOpenAIChat
	if got := schemaMode(provcore.FormatAnthropic, provcore.FormatOpenAI, ep, bridge); got != "translated" {
		t.Fatalf("schemaMode anthropic→openai = %q, want translated", got)
	}
	if got := schemaMode(provcore.FormatGemini, provcore.FormatAnthropic, ep, bridge); got != "translated" {
		t.Fatalf("schemaMode gemini→anthropic = %q, want translated", got)
	}
}

func TestFilterCompatibleTargets_Passthrough_Legacy(t *testing.T) {
	targets := []routingcore.RoutingTarget{
		{ProviderID: "p1", ProviderName: "anthropic", AdapterType: "anthropic", ModelID: "m1"},
		{ProviderID: "p2", ProviderName: "openai", AdapterType: "openai", ModelID: "m2"},
	}
	compat, rejected := filterCompatibleTargets(provcore.FormatAnthropic, targets, typology.WireShapeOpenAIChat, nil)
	if len(compat) != 1 || compat[0].ProviderName != "anthropic" {
		t.Fatalf("compat = %+v, want anthropic only", compat)
	}
	if len(rejected) != 1 || rejected[0].ProviderName != "openai" {
		t.Fatalf("rejected = %+v, want openai only", rejected)
	}
	if rejected[0].ProviderFormat != provcore.FormatOpenAI {
		t.Fatalf("rejected ProviderFormat = %q, want openai", rejected[0].ProviderFormat)
	}
}

func TestFilterCompatibleTargets_WithHub_AnthropicToOpenAI(t *testing.T) {
	bridge := canonicalbridge.New(provbuiltins.SchemaCodecs(nil))
	targets := []routingcore.RoutingTarget{
		{ProviderID: "p1", ProviderName: "anthropic", AdapterType: "anthropic", ModelID: "m1"},
		{ProviderID: "p2", ProviderName: "openai", AdapterType: "openai", ModelID: "m2"},
	}
	compat, rejected := filterCompatibleTargets(provcore.FormatAnthropic, targets, typology.WireShapeOpenAIChat, bridge)
	if len(compat) != 2 {
		t.Fatalf("compat len = %d, want 2 (hub allows anthropic→openai)", len(compat))
	}
	if len(rejected) != 0 {
		t.Fatalf("rejected = %+v, want empty", rejected)
	}
}

func TestFilterCompatibleTargets_OpenAIIngress_AcceptsAll(t *testing.T) {
	targets := []routingcore.RoutingTarget{
		{ProviderName: "anthropic", AdapterType: "anthropic"},
		{ProviderName: "openai", AdapterType: "openai"},
		{ProviderName: "gemini", AdapterType: "gemini"},
	}
	compat, rejected := filterCompatibleTargets(provcore.FormatOpenAI, targets, typology.WireShapeOpenAIChat, nil)
	if len(compat) != 3 {
		t.Fatalf("compat len = %d, want 3", len(compat))
	}
	if len(rejected) != 0 {
		t.Fatalf("rejected = %+v, want empty", rejected)
	}
}

func TestFilterCompatibleTargets_UnknownProviderDropped(t *testing.T) {
	targets := []routingcore.RoutingTarget{
		{ProviderName: "mystery-cloud", AdapterType: "unknown-adapter"},
		{ProviderName: "openai", AdapterType: "openai"},
	}
	compat, rejected := filterCompatibleTargets(provcore.FormatOpenAI, targets, typology.WireShapeOpenAIChat, nil)
	if len(compat) != 1 || compat[0].ProviderName != "openai" {
		t.Fatalf("compat = %+v, want openai only", compat)
	}
	if len(rejected) != 0 {
		t.Fatalf("rejected = %+v, want empty (unknown adapter types are dropped)", rejected)
	}
}
