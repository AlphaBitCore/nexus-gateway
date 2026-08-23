package strategies

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

func BenchmarkStrategyEvaluate_Single(b *testing.B) {
	reg := NewStrategyRegistry()
	lookup := func(_ context.Context, providerID, modelID string) (*core.RoutingTarget, error) {
		return &core.RoutingTarget{ProviderID: providerID, ModelID: modelID, ProviderName: providerID}, nil
	}
	RegisterAllStrategies(reg, lookup, nil, nil)

	node := core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}
	ctx := &core.RoutingContext{}

	for b.Loop() {
		var trace []core.TraceEntry
		_, _ = reg.Evaluate(context.Background(), node, ctx, &trace)
	}
}

func BenchmarkStrategyEvaluate_Fallback(b *testing.B) {
	reg := NewStrategyRegistry()
	lookup := func(_ context.Context, providerID, modelID string) (*core.RoutingTarget, error) {
		return &core.RoutingTarget{ProviderID: providerID, ModelID: modelID, ProviderName: providerID}, nil
	}
	RegisterAllStrategies(reg, lookup, nil, nil)

	node := core.StrategyNode{
		Type: "fallback",
		Targets: []core.StrategyNode{
			{Type: "single", ProviderID: "openai", ModelID: "gpt-4"},
			{Type: "single", ProviderID: "anthropic", ModelID: "claude-3"},
			{Type: "single", ProviderID: "google", ModelID: "gemini-pro"},
		},
	}
	ctx := &core.RoutingContext{}

	for b.Loop() {
		var trace []core.TraceEntry
		_, _ = reg.Evaluate(context.Background(), node, ctx, &trace)
	}
}

// filterByCapability sits on the routing path, and the image dimension moved
// from reading Features to reading InputModalities. The claim that this is
// perf-neutral was written as an assumption; this measures it instead.
//
// The candidate pool is sized like the real catalogue (~50 chat models) and
// the arrays like real rows: a short feature list, a short modality list. Both
// dimensions are exercised, because the tool dimension still reads Features
// and a regression there would be invisible if only the image path ran.
func benchCandidates(n int) []core.SmartModelRow {
	rows := make([]core.SmartModelRow, 0, n)
	for i := range n {
		r := core.SmartModelRow{
			ModelID:   "m",
			ModelCode: "model",
			Features:  []string{"streaming", "function_calling", "json_mode"},
		}
		// Half the catalogue takes images, as the live one does.
		if i%2 == 0 {
			r.InputModalities = []string{"text", "image"}
		} else {
			r.InputModalities = []string{"text"}
		}
		rows = append(rows, r)
	}
	return rows
}

func BenchmarkFilterByCapability_ImageNeed(b *testing.B) {
	rows := benchCandidates(50)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		kept, _, _ := filterByCapability(rows, false, false, false, reqText|reqImage, modsOf(reqText|reqImage))
		if len(kept) == 0 {
			b.Fatal("pool emptied")
		}
	}
}

func BenchmarkFilterByCapability_ToolNeed(b *testing.B) {
	rows := benchCandidates(50)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		kept, _, _ := filterByCapability(rows, true, false, false, reqText, modsOf(reqText))
		if len(kept) == 0 {
			b.Fatal("pool emptied")
		}
	}
}

func BenchmarkFilterByCapability_BothNeeds(b *testing.B) {
	rows := benchCandidates(50)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		kept, _, _ := filterByCapability(rows, true, false, false, reqText|reqImage, modsOf(reqText|reqImage))
		if len(kept) == 0 {
			b.Fatal("pool emptied")
		}
	}
}

// The before/after pair. `filterByCapabilityLegacy` is the implementation this
// replaced — the image dimension read Features["vision"]. It exists only here,
// so the comparison is against the real prior code rather than a description
// of it.
//
// The two are fed shape-equivalent pools: the legacy one gets "vision" in
// Features where the current one gets "image" in InputModalities, so the same
// half of the catalogue survives each filter and the measurement is of the
// lookup, not of how many rows happened to be dropped.
func filterByCapabilityLegacy(candidates []core.SmartModelRow, needsVision, needsTools bool) (kept []core.SmartModelRow, dropped int, skippedDims []string) {
	kept = candidates
	for _, dim := range []struct {
		needed  bool
		feature string
	}{
		{needsVision, "vision"},
		{needsTools, featureFunctionCalling},
	} {
		if !dim.needed {
			continue
		}
		var pass []core.SmartModelRow
		for _, c := range kept {
			if len(c.Features) == 0 || containsFeature(c.Features, dim.feature) {
				pass = append(pass, c)
			}
		}
		if len(pass) == 0 {
			skippedDims = append(skippedDims, dim.feature)
			continue
		}
		dropped += len(kept) - len(pass)
		kept = pass
	}
	return kept, dropped, skippedDims
}

// Legacy pool: the vision signal lives in Features, and the modality arrays
// carry the same number of entries so slice sizes match.
func benchCandidatesLegacy(n int) []core.SmartModelRow {
	rows := make([]core.SmartModelRow, 0, n)
	for i := range n {
		r := core.SmartModelRow{
			ModelID:         "m",
			ModelCode:       "model",
			Features:        []string{"streaming", "function_calling", "json_mode"},
			InputModalities: []string{"text"},
		}
		if i%2 == 0 {
			r.Features = append(r.Features, "vision")
			r.InputModalities = []string{"text", "image"}
		}
		rows = append(rows, r)
	}
	return rows
}

func BenchmarkFilterByCapability_ImageNeed_Legacy(b *testing.B) {
	rows := benchCandidatesLegacy(50)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		kept, _, _ := filterByCapabilityLegacy(rows, true, false)
		if len(kept) == 0 {
			b.Fatal("pool emptied")
		}
	}
}

func BenchmarkFilterByCapability_ImageNeed_Current(b *testing.B) {
	rows := benchCandidatesLegacy(50) // same pool, both signals present
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		kept, _, _ := filterByCapability(rows, false, false, false, reqText|reqImage, modsOf(reqText|reqImage))
		if len(kept) == 0 {
			b.Fatal("pool emptied")
		}
	}
}
