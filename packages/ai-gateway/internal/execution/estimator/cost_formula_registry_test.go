// Package estimator_test — cost_formula_registry_test.go covers the
// per-endpoint cost formula dispatch table keyed by canonical
// typology.EndpointKind strings.
//
// Named failure modes tested:
//   - "chat" → chatCostFormula → uses both prompt + completion tokens
//   - "embeddings" → embeddingsCostFormula → prompt-only, completion ignored
//   - "image_generation"/"tts"/"stt" → modality formulas → price by
//     Images/InputChars/AudioSeconds at per-1M-unit rates; usage tokens
//     win when present (token-priced models); no WARN-fallback
//   - unknown endpoint → safe default (chatCostFormula) + one WARN
//   - RegisterFormula  → custom formula replaces built-in for that endpoint
//   - BillableUnits zero → cost zero; every field has a consuming formula
//   - EmbeddingsCostFormula: 1000 tokens × $0.02/M = $0.00002
package estimator_test

import (
	"bytes"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/estimator"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
)

func ptr64(f float64) *float64 { return &f }

// TestLookup_knownEndpoints verifies that built-in endpoints resolve to the
// correct formula and produce expected cost totals.
func TestLookup_knownEndpoints(t *testing.T) {
	chatPrices := metrics.ModelPrices{
		InputUsdPerM:  ptr64(2.5),  // $2.50 / 1M input
		OutputUsdPerM: ptr64(10.0), // $10.00 / 1M output
	}
	embPrices := metrics.ModelPrices{
		InputUsdPerM:  ptr64(0.02), // $0.02 / 1M input
		OutputUsdPerM: ptr64(0.0),  // embeddings have no output price
	}

	cases := []struct {
		endpoint        string
		units           estimator.BillableUnits
		prices          metrics.ModelPrices
		wantTotalApprox float64
		desc            string
	}{
		{
			endpoint: "chat",
			units:    estimator.BillableUnits{PromptTokens: 1000, CompletionTokens: 500},
			prices:   chatPrices,
			// 1000*2.5/1e6 + 500*10/1e6 = 0.0025 + 0.005 = 0.0075
			wantTotalApprox: 0.0075,
			desc:            "chat: prompt + completion",
		},
		{
			endpoint: "embeddings",
			units:    estimator.BillableUnits{PromptTokens: 1000, CompletionTokens: 99}, // completion must be ignored
			prices:   embPrices,
			// 1000*0.02/1e6 = 0.00002
			wantTotalApprox: 0.00002,
			desc:            "embeddings: prompt-only; completion tokens ignored",
		},
		{
			endpoint:        "embeddings",
			units:           estimator.BillableUnits{PromptTokens: 0},
			prices:          embPrices,
			wantTotalApprox: 0.0,
			desc:            "embeddings zero tokens → zero cost",
		},
		{
			endpoint: "unknown-future-endpoint",
			units:    estimator.BillableUnits{PromptTokens: 1000, CompletionTokens: 500},
			prices:   chatPrices,
			// Safe default: falls back to chat formula
			wantTotalApprox: 0.0075,
			desc:            "unknown endpoint falls back to chat formula",
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			formula := estimator.Lookup(tc.endpoint)
			if formula == nil {
				t.Fatal("Lookup returned nil formula")
			}
			cost := formula(tc.units, tc.prices)
			if math.Abs(cost.Total-tc.wantTotalApprox) > 1e-10 {
				t.Errorf("cost.Total = %.10f, want %.10f", cost.Total, tc.wantTotalApprox)
			}
		})
	}
}

// TestEmbeddingsCostFormula_completionIgnored verifies that completion tokens
// do not contribute to the embeddings cost formula (SDD T3.5: embeddings
// populate only PromptTokens; completion tokens must be zero from codec).
func TestEmbeddingsCostFormula_completionIgnored(t *testing.T) {
	prices := metrics.ModelPrices{
		InputUsdPerM:  ptr64(0.13),  // text-embedding-3-small pricing
		OutputUsdPerM: ptr64(100.0), // very high — must not appear in result
	}
	units := estimator.BillableUnits{PromptTokens: 1000, CompletionTokens: 9999}
	cost := estimator.Lookup("embeddings")(units, prices)
	// Should only be 1000 * 0.13 / 1e6 = 0.00013
	want := 0.00013
	if math.Abs(cost.Total-want) > 1e-10 {
		t.Errorf("embeddings cost = %.10f, want %.10f (completion tokens leaked into formula)", cost.Total, want)
	}
}

// TestRegisterFormula_overridesBuiltin verifies that RegisterFormula lets
// future epics inject a custom formula without modifying the dispatcher.
func TestRegisterFormula_overridesBuiltin(t *testing.T) {
	const testEndpoint = "_test_custom_endpoint_e62"
	called := false
	custom := func(u estimator.BillableUnits, p metrics.ModelPrices) metrics.Cost {
		called = true
		return metrics.Cost{Total: 42.0}
	}
	estimator.RegisterFormula(testEndpoint, custom)
	// No cleanup needed: test endpoint key is private to this test
	// and will not conflict with other tests.

	formula := estimator.Lookup(testEndpoint)
	cost := formula(estimator.BillableUnits{}, metrics.ModelPrices{})
	if !called {
		t.Error("registered custom formula was not called")
	}
	if cost.Total != 42.0 {
		t.Errorf("custom formula result = %.1f, want 42.0", cost.Total)
	}
}

// TestLookup_chatCostMatchesEstimatedCostUSD validates that the chat
// formula produces the same result as the previous estimatedCostUSD
// helper so no regression in existing chat cost stamping.
func TestLookup_chatCostMatchesEstimatedCostUSD(t *testing.T) {
	inPM := 2.5
	outPM := 10.0
	promptTok := int64(1300)
	completionTok := int64(700)

	// Old formula: float64(p)*inPM/1e6 + float64(c)*outPM/1e6
	want := float64(promptTok)*inPM/1_000_000 + float64(completionTok)*outPM/1_000_000

	units := estimator.BillableUnits{
		PromptTokens:     int(promptTok),
		CompletionTokens: int(completionTok),
	}
	cost := estimator.Lookup("chat")(units, metrics.ModelPrices{
		InputUsdPerM:  &inPM,
		OutputUsdPerM: &outPM,
	})
	if math.Abs(cost.Total-want) > 1e-10 {
		t.Errorf("chat formula = %.10f, want %.10f (regression vs estimatedCostUSD)", cost.Total, want)
	}
}

// TestLookup_unregisteredEndpoint_WarnsOnce is the visibility
// assertion: an unregistered endpoint must (a) still resolve to a usable
// (chat) formula and (b) emit exactly one WARN log naming the endpoint, no
// matter how many times it is looked up — so the silent token-mispricing
// fallback becomes observable without flooding the log.
func TestLookup_unregisteredEndpoint_WarnsOnce(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// A unique endpoint string so the process-lifetime dedup map has not
	// already recorded it from another test.
	const ep = "_f0234_unregistered_endpoint_probe"

	for range 5 {
		formula := estimator.Lookup(ep)
		if formula == nil {
			t.Fatalf("Lookup(%q) returned nil; expected chat-formula fallback", ep)
		}
		// Fallback must price like the chat formula (prompt + completion).
		inPM, outPM := 2.0, 4.0
		cost := formula(estimator.BillableUnits{PromptTokens: 1000, CompletionTokens: 500}, metrics.ModelPrices{
			InputUsdPerM:  &inPM,
			OutputUsdPerM: &outPM,
		})
		want := 1000*inPM/1e6 + 500*outPM/1e6
		if math.Abs(cost.Total-want) > 1e-10 {
			t.Errorf("fallback cost = %.10f, want chat-formula %.10f", cost.Total, want)
		}
	}

	logged := buf.String()
	if !strings.Contains(logged, ep) {
		t.Errorf("expected a WARN naming endpoint %q; log was: %q", ep, logged)
	}
	if got := strings.Count(logged, ep); got != 1 {
		t.Errorf("expected exactly 1 WARN for endpoint %q across 5 lookups, got %d; log was: %q", ep, got, logged)
	}
}

// TestBillableUnits_EveryFieldConsumed locks the no-dead-surface contract:
// every BillableUnits field is consumed by at least one registered formula.
// Token fields price through chat; each modality field prices through its
// own kind's formula. A field added without a consuming formula prices its
// units to zero everywhere and fails here — and the reflective completeness
// check below forces the new field INTO this case list, so the lock cannot
// be silently bypassed by simply not adding a case.
func TestBillableUnits_EveryFieldConsumed(t *testing.T) {
	inPM, outPM := 1000.0, 2000.0
	audioInPM, audioOutPM := 32000.0, 64000.0
	prices := metrics.ModelPrices{
		InputUsdPerM: &inPM, OutputUsdPerM: &outPM,
		// Audio rates so the realtime split fields price non-zero; the
		// cached fields exercise their nil-cache fallbacks (cached-text →
		// InputUsdPerM, cached-audio → AudioInputUsdPerM).
		AudioInputUsdPerM: &audioInPM, AudioOutputUsdPerM: &audioOutPM,
	}

	cases := []struct {
		field    string
		endpoint string
		units    estimator.BillableUnits
	}{
		{"PromptTokens", "chat", estimator.BillableUnits{PromptTokens: 10}},
		{"CompletionTokens", "chat", estimator.BillableUnits{CompletionTokens: 20}},
		{"Images", "image_generation", estimator.BillableUnits{Images: 1}},
		{"AudioSeconds", "stt", estimator.BillableUnits{AudioSeconds: 30}},
		{"InputChars", "tts", estimator.BillableUnits{InputChars: 500}},
		{"SearchUnits", "rerank", estimator.BillableUnits{SearchUnits: 3}},
		{"VideoSeconds", "video_generation", estimator.BillableUnits{VideoSeconds: 8}},
		{"AudioInputTokens", "realtime", estimator.BillableUnits{AudioInputTokens: 100}},
		{"AudioOutputTokens", "realtime", estimator.BillableUnits{AudioOutputTokens: 100}},
		{"CachedTextReadTokens", "realtime", estimator.BillableUnits{CachedTextReadTokens: 100}},
		{"CachedAudioReadTokens", "realtime", estimator.BillableUnits{CachedAudioReadTokens: 100}},
	}

	// Reflective completeness: every struct field must appear in the case
	// list above. Without this, a future field could land with a formula but
	// no case here, and the lock would silently stop covering it.
	covered := map[string]bool{}
	for _, tc := range cases {
		covered[tc.field] = true
	}
	rt := reflect.TypeOf(estimator.BillableUnits{})
	for i := range rt.NumField() {
		if name := rt.Field(i).Name; !covered[name] {
			t.Errorf("BillableUnits field %s has no consumption case in this test; add one proving a registered formula consumes it", name)
		}
	}

	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			cost := estimator.Lookup(tc.endpoint)(tc.units, prices)
			if cost.Total <= 0 {
				t.Errorf("field %s is dead surface: units %+v priced to %.12f via %q; every BillableUnits field must have a consuming formula",
					tc.field, tc.units, cost.Total, tc.endpoint)
			}
		})
	}
}

// TestLookup_modalityEndpoints verifies the multimodal cost migration:
// image / tts / stt resolve to their own formulas and price by modality
// units at real catalog rates — not through the chat token fallback (which
// would price these unit-only requests to zero).
func TestLookup_modalityEndpoints(t *testing.T) {
	cases := []struct {
		endpoint        string
		units           estimator.BillableUnits
		inputUsdPerM    float64
		wantTotalApprox float64
		desc            string
	}{
		{
			endpoint:     "image_generation",
			units:        estimator.BillableUnits{Images: 2},
			inputUsdPerM: 40000, // dall-e-3 standard 1024²: $0.04/image = $40000 per 1M images
			// 2 / 1e6 * 40000 = 0.08
			wantTotalApprox: 0.08,
			desc:            "image: 2 dall-e-3 images at $0.04 each",
		},
		{
			endpoint:     "tts",
			units:        estimator.BillableUnits{InputChars: 1000},
			inputUsdPerM: 15, // tts-1: $15.00 per 1M characters
			// 1000 / 1e6 * 15 = 0.015
			wantTotalApprox: 0.015,
			desc:            "tts: 1000 chars at tts-1 rate",
		},
		{
			endpoint:     "stt",
			units:        estimator.BillableUnits{AudioSeconds: 60},
			inputUsdPerM: 100, // whisper-1: $0.006/min = $0.0001/sec = $100 per 1M seconds
			// 60 / 1e6 * 100 = 0.006
			wantTotalApprox: 0.006,
			desc:            "stt: 60s of audio at whisper-1 rate",
		},
		{
			endpoint:     "stt",
			units:        estimator.BillableUnits{AudioSeconds: 90.5},
			inputUsdPerM: 100,
			// fractional seconds must not truncate: 90.5 / 1e6 * 100 = 0.00905
			wantTotalApprox: 0.00905,
			desc:            "stt: fractional seconds price without truncation",
		},
		{
			endpoint:     "video_generation",
			units:        estimator.BillableUnits{VideoSeconds: 8},
			inputUsdPerM: 100000, // sora-2: $0.10/s = $100000 per 1M seconds
			// 8 / 1e6 * 100000 = 0.8
			wantTotalApprox: 0.8,
			desc:            "video: 8s of sora-2 at $0.10/s",
		},
		{
			endpoint:     "video_generation",
			units:        estimator.BillableUnits{VideoSeconds: 7.5},
			inputUsdPerM: 100000,
			// fractional seconds must not truncate: 7.5 / 1e6 * 100000 = 0.75
			wantTotalApprox: 0.75,
			desc:            "video: fractional seconds price without truncation",
		},
		{
			endpoint:        "image_generation",
			units:           estimator.BillableUnits{},
			inputUsdPerM:    40000,
			wantTotalApprox: 0.0,
			desc:            "image: zero units + zero tokens → zero cost (stamping site owns the WARN)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			prices := metrics.ModelPrices{InputUsdPerM: ptr64(tc.inputUsdPerM)}
			cost := estimator.Lookup(tc.endpoint)(tc.units, prices)
			if math.Abs(cost.Total-tc.wantTotalApprox) > 1e-10 {
				t.Errorf("cost.Total = %.10f, want %.10f", cost.Total, tc.wantTotalApprox)
			}
			// Modality unit pricing is input-side only: the artifact output is
			// binary, never token-priced, so Output must stay 0 on the unit path.
			if cost.Output != 0 {
				t.Errorf("unit-path cost.Output = %.10f, want 0 (binary artifact output is never token-priced)", cost.Output)
			}
		})
	}
}

// TestModalityFormulas_usageTokensWin verifies the dispatch rule for
// token-priced modality models (gpt-image-1, gpt-4o-transcribe, token TTS):
// when provider usage reports tokens, the token path is authoritative and
// the modality unit is ignored — the per-model InputUsdPerM then means
// per-1M-tokens, so mixing both paths would double-price.
func TestModalityFormulas_usageTokensWin(t *testing.T) {
	inPM, outPM := 5.0, 40.0 // per-1M-token pricing for a token-usage model
	prices := metrics.ModelPrices{InputUsdPerM: &inPM, OutputUsdPerM: &outPM}
	wantTokenCost := 100*inPM/1e6 + 4000*outPM/1e6 // 0.0005 + 0.16

	for _, endpoint := range []string{"image_generation", "tts", "stt", "rerank", "video_generation"} {
		t.Run(endpoint, func(t *testing.T) {
			units := estimator.BillableUnits{
				PromptTokens:     100,
				CompletionTokens: 4000,
				// Modality units also present — must NOT add to the cost.
				Images:       2,
				AudioSeconds: 60,
				InputChars:   1000,
				SearchUnits:  3,
				VideoSeconds: 8,
			}
			cost := estimator.Lookup(endpoint)(units, prices)
			if math.Abs(cost.Total-wantTokenCost) > 1e-10 {
				t.Errorf("%s with usage tokens = %.10f, want token-only %.10f (modality units must not double-price)",
					endpoint, cost.Total, wantTokenCost)
			}
		})
	}
}

// TestLookup_modalityEndpoints_noFallbackWarn asserts the registration
// closed the mispricing gap observably: looking up the three modality
// endpoints emits NO "falling back to chat formula" WARN (they are
// registered), while genuinely unregistered kinds still warn (covered by
// TestLookup_unregisteredEndpoint_WarnsOnce).
func TestLookup_modalityEndpoints_noFallbackWarn(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	for _, endpoint := range []string{"image_generation", "tts", "stt", "rerank", "video_generation"} {
		estimator.Lookup(endpoint)
	}
	if logged := buf.String(); strings.Contains(logged, "falling back") {
		t.Errorf("registered modality endpoints must not WARN-fallback; log was: %q", logged)
	}
}

// TestRerankCostFormula asserts both rerank pricing modes: search-unit-priced
// (Cohere reports search_units → priced via InputUsdPerM per 1M units) and
// token-priced (Voyage reports total_tokens stamped as PromptTokens → token
// path, NOT $0). The token case is the C1 regression guard.
func TestRerankCostFormula(t *testing.T) {
	t.Run("cohere_search_units", func(t *testing.T) {
		// Cohere rerank-3.5 at $2.00 / 1K searches → $2000 / 1M search units.
		inPM := 2000.0
		prices := metrics.ModelPrices{InputUsdPerM: &inPM}
		units := estimator.BillableUnits{SearchUnits: 3} // 3 search units
		cost := estimator.Lookup("rerank")(units, prices)
		want := 3 * inPM / 1e6 // 0.006
		if math.Abs(cost.Total-want) > 1e-10 {
			t.Fatalf("cohere rerank cost = %.10f, want %.10f", cost.Total, want)
		}
	})
	t.Run("voyage_tokens_not_zero", func(t *testing.T) {
		inPM := 0.05 // $0.05 / 1M tokens
		prices := metrics.ModelPrices{InputUsdPerM: &inPM}
		// Voyage stamps total_tokens as PromptTokens; SearchUnits is 0.
		units := estimator.BillableUnits{PromptTokens: 1000, SearchUnits: 0}
		cost := estimator.Lookup("rerank")(units, prices)
		want := 1000 * inPM / 1e6 // 0.00005
		if cost.Total <= 0 {
			t.Fatalf("voyage rerank priced at $0 — the C1 regression (token path not taken)")
		}
		if math.Abs(cost.Total-want) > 1e-12 {
			t.Fatalf("voyage rerank cost = %.12f, want %.12f", cost.Total, want)
		}
	})
	t.Run("no_units_zero", func(t *testing.T) {
		inPM := 2000.0
		prices := metrics.ModelPrices{InputUsdPerM: &inPM}
		cost := estimator.Lookup("rerank")(estimator.BillableUnits{}, prices)
		if cost.Total != 0 {
			t.Fatalf("rerank with no units = %.10f, want 0", cost.Total)
		}
	})
}

// TestRealtimeCostFormula asserts the six-component realtime pricing:
// uncached text + cached text + uncached audio + cached audio on the input
// side, text + audio on the output side, each at its own rate, with the
// documented nil-rate fallbacks (cached-text → text input rate, cached-audio
// → audio input rate, primary audio rates → $0 component).
func TestRealtimeCostFormula(t *testing.T) {
	// gpt-realtime-2.1 published rates (USD per 1M tokens).
	textIn, textOut, cachedText := 4.0, 24.0, 0.40
	audioIn, audioOut, cachedAudio := 32.0, 64.0, 0.40

	t.Run("six_components_exact", func(t *testing.T) {
		prices := metrics.ModelPrices{
			InputUsdPerM: &textIn, OutputUsdPerM: &textOut,
			CachedInputReadUsdPerM: &cachedText,
			AudioInputUsdPerM:      &audioIn, AudioOutputUsdPerM: &audioOut,
			CachedAudioInputReadUsdPerM: &cachedAudio,
		}
		units := estimator.BillableUnits{
			PromptTokens:          1000, // uncached text in
			CachedTextReadTokens:  500,
			AudioInputTokens:      2000, // uncached audio in
			CachedAudioReadTokens: 300,
			CompletionTokens:      200, // text out
			AudioOutputTokens:     1500,
		}
		cost := estimator.Lookup("realtime")(units, prices)
		wantIn := (1000*textIn + 2000*audioIn) / 1e6          // 0.068
		wantCache := (500*cachedText + 300*cachedAudio) / 1e6 // 0.00032
		wantOut := (200*textOut + 1500*audioOut) / 1e6        // 0.1008
		if math.Abs(cost.UncachedInput-wantIn) > 1e-12 {
			t.Fatalf("UncachedInput = %.12f, want %.12f", cost.UncachedInput, wantIn)
		}
		if math.Abs(cost.CacheRead-wantCache) > 1e-12 {
			t.Fatalf("CacheRead = %.12f, want %.12f", cost.CacheRead, wantCache)
		}
		if math.Abs(cost.Output-wantOut) > 1e-12 {
			t.Fatalf("Output = %.12f, want %.12f", cost.Output, wantOut)
		}
		if math.Abs(cost.Total-(wantIn+wantCache+wantOut)) > 1e-12 {
			t.Fatalf("Total = %.12f, want %.12f", cost.Total, wantIn+wantCache+wantOut)
		}
	})
	t.Run("cached_rate_fallbacks", func(t *testing.T) {
		// Nil cached rates fall back to the matching PRIMARY input rate
		// ("no discount"), per the shipped text-cache column contract.
		prices := metrics.ModelPrices{
			InputUsdPerM: &textIn, OutputUsdPerM: &textOut,
			AudioInputUsdPerM: &audioIn, AudioOutputUsdPerM: &audioOut,
		}
		units := estimator.BillableUnits{CachedTextReadTokens: 100, CachedAudioReadTokens: 100}
		cost := estimator.Lookup("realtime")(units, prices)
		want := (100*textIn + 100*audioIn) / 1e6 // no discount: full input rates
		if math.Abs(cost.CacheRead-want) > 1e-12 {
			t.Fatalf("CacheRead fallback = %.12f, want %.12f (cached-text→textIn, cached-audio→audioIn)", cost.CacheRead, want)
		}
	})
	t.Run("missing_primary_audio_prices_component_zero", func(t *testing.T) {
		// A model row without audio rates prices ONLY the audio components
		// $0 — the text components still bill. (The relay refuses such a
		// model at upgrade under an enforced quota; this arm is the
		// un-enforced spend-visibility gap.)
		prices := metrics.ModelPrices{InputUsdPerM: &textIn, OutputUsdPerM: &textOut}
		units := estimator.BillableUnits{
			PromptTokens: 1000, CompletionTokens: 200,
			AudioInputTokens: 5000, AudioOutputTokens: 5000, CachedAudioReadTokens: 500,
		}
		cost := estimator.Lookup("realtime")(units, prices)
		want := (1000*textIn + 200*textOut) / 1e6
		if math.Abs(cost.Total-want) > 1e-12 {
			t.Fatalf("Total = %.12f, want %.12f (audio components must price $0 when primary audio rates are nil)", cost.Total, want)
		}
	})
	t.Run("no_units_zero", func(t *testing.T) {
		prices := metrics.ModelPrices{InputUsdPerM: &textIn, AudioInputUsdPerM: &audioIn}
		if cost := estimator.Lookup("realtime")(estimator.BillableUnits{}, prices); cost.Total != 0 {
			t.Fatalf("realtime with no units = %.12f, want 0", cost.Total)
		}
	})
}
