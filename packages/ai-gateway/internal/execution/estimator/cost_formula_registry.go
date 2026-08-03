// Package estimator — cost_formula_registry.go implements the per-endpoint
// cost formula dispatch table.
//
// Each endpoint typology (chat, embeddings, …) owns its own formula.
// New endpoint types call RegisterFormula on init to add their formula
// without changing the dispatcher in proxy.go.
package estimator

import (
	"log/slog"
	"sync"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// BillableUnits captures the per-endpoint resource consumption the cost
// formula consumes.
//
// The two token fields are stamped by every chat/embeddings cost path
// (proxy_cache_hits.go, proxy_responses.go, proxy_upstream.go). Reasoning
// tokens are already folded into CompletionTokens by the upstream usage
// normalizer (all three frontier providers bill reasoning at the output
// rate — see metrics.CalculateCost), so a separate ReasoningTokens unit
// would be double-priced or dead.
//
// The modality fields are stamped by the multimodal ingress paths and
// consumed by the per-kind formulas below. Every modality formula prefers
// provider-reported usage TOKENS when present (token-priced models such as
// gpt-image-1 / gpt-4o-transcribe report authoritative usage) and falls
// back to its modality unit otherwise:
//   - Images       — image_generation: generated image count (per-image
//     priced models such as dall-e-*).
//   - AudioSeconds — stt: billable input-audio duration in seconds
//     (whisper-shape models report `duration`; fractional, hence float).
//   - InputChars   — tts: synthesized input-text character count
//     (per-character priced models such as tts-1).
type BillableUnits struct {
	PromptTokens     int
	CompletionTokens int
	Images           int
	AudioSeconds     float64
	InputChars       int
	// SearchUnits — rerank: provider-reported billable search units (Cohere
	// reports meta.billed_units.search_units; 1 query + up to 100 documents =
	// 1 unit). Voyage rerank prices by tokens instead and flows through the
	// token path via PromptTokens.
	SearchUnits int
	// VideoSeconds — video_generation: seconds of generated video (per-second
	// priced models such as sora-2; resolution-tiered pricing is expressed as
	// separate catalog model entries, not a schema extension). Float because
	// a provider may report fractional realized durations.
	VideoSeconds float64
	// Realtime token splits — a single realtime response bills six
	// components simultaneously at different rates, which the
	// one-unit-per-model-row trick cannot express. For realtime rows,
	// PromptTokens carries UNCACHED TEXT input and CompletionTokens carries
	// TEXT output (per-endpoint unit reinterpretation, like AudioSeconds /
	// InputChars above); the four fields below carry the remaining splits
	// from response.done.usage's input/output_token_details.
	AudioInputTokens     int // uncached audio input tokens
	AudioOutputTokens    int // audio output tokens
	CachedTextReadTokens int // cached text input tokens (read)
	// CachedAudioReadTokens — cached audio input tokens (read). When the
	// provider omits cached_tokens_details, the extractor attributes all
	// cached tokens to TEXT cache (audio-attribution would overbill;
	// text-attribution underbills at most the audio/text cache-rate delta).
	CachedAudioReadTokens int
}

// CostFormula returns the estimated USD cost for a request given its
// billable units and the model's configured pricing snapshot.
type CostFormula func(units BillableUnits, prices metrics.ModelPrices) metrics.Cost

// formulaRegistry is the process-wide endpoint → formula map. Access is
// guarded by formulaMu. Populated at package init with the built-in
// formulas keyed by the canonical typology.EndpointKind string vocabulary;
// new endpoint types add entries via RegisterFormula.
var (
	formulaMu  sync.RWMutex
	formulaMap = map[string]CostFormula{
		string(typology.EndpointKindChat):            chatCostFormula,
		string(typology.EndpointKindEmbeddings):      embeddingsCostFormula,
		string(typology.EndpointKindImageGeneration): imageCostFormula,
		string(typology.EndpointKindTTS):             ttsCostFormula,
		string(typology.EndpointKindSTT):             sttCostFormula,
		string(typology.EndpointKindRerank):          rerankCostFormula,
		string(typology.EndpointKindVideoGeneration): videoCostFormula,
		string(typology.EndpointKindRealtime):        realtimeCostFormula,
	}
)

// unregisteredWarned dedupes the missing-formula warning to one log line
// per endpoint string for the process lifetime, so a high-QPS stream of
// requests against a gap in the registry does not flood the log while
// still surfacing the gap exactly once.
var unregisteredWarned sync.Map // endpoint string -> struct{}

// Lookup returns the CostFormula registered for the given endpoint kind
// string. When no formula is found, the chat formula is returned as a
// safe default so callers are never blocked on missing entries — but the
// fallback is no longer silent: the first lookup of an unregistered
// endpoint is logged at WARN so an operator can see that e.g. batch
// traffic is being token-mispriced through the chat formula until a
// dedicated formula is registered. Endpoint strings match
// audit.Record.EndpointType — canonical typology kind values ("chat",
// "embeddings", "rerank", "stt", "tts", "image_generation",
// "video_generation", "realtime"; unregistered kinds such as "batch" take
// the warned fallback).
func Lookup(endpoint string) CostFormula {
	formulaMu.RLock()
	f, ok := formulaMap[endpoint]
	formulaMu.RUnlock()
	if ok {
		return f
	}
	if _, loaded := unregisteredWarned.LoadOrStore(endpoint, struct{}{}); !loaded {
		slog.Warn("estimator: no cost formula registered for endpoint; falling back to chat formula (tokens may be mispriced)",
			"endpoint", endpoint)
	}
	return chatCostFormula
}

// RegisterFormula registers a custom CostFormula for the given endpoint
// kind. New endpoint types call it from their init() so the dispatcher
// in proxy.go remains unchanged. Re-registration replaces the existing
// formula for that kind. Passing a nil formula is a no-op.
func RegisterFormula(endpoint string, f CostFormula) {
	if f == nil {
		return
	}
	formulaMu.Lock()
	defer formulaMu.Unlock()
	formulaMap[endpoint] = f
}

// chatCostFormula prices chat / completions / responses requests via the
// standard token-bucket arithmetic: prompt × inputPrice + completion ×
// outputPrice. Wraps costForTokens so both the estimator dry-run path
// and the proxy cost-stamp path share identical arithmetic.
func chatCostFormula(u BillableUnits, prices metrics.ModelPrices) metrics.Cost {
	return costForTokens(u.PromptTokens, u.CompletionTokens, prices)
}

// embeddingsCostFormula prices embedding requests. Embeddings have no
// completion tokens; cost = (promptTokens / 1M) × inputPricePerMillion.
// CompletionTokens is ignored.
func embeddingsCostFormula(u BillableUnits, prices metrics.ModelPrices) metrics.Cost {
	return costForTokens(u.PromptTokens, 0, prices)
}

// Modality formulas — image_generation / tts / stt.
//
// Price-field semantic: ModelPrices.InputUsdPerM is "USD per million
// billable input units" where the UNIT is the modality's own — tokens for
// token-usage models, images for per-image models, characters for
// per-character TTS, audio seconds for per-second STT. A given model row is
// unambiguous: a model either reports token usage (price configured per 1M
// tokens) or it does not (price configured per 1M modality units, e.g.
// dall-e-3 standard 1024² at $0.04/image → InputUsdPerM = 40000).
// Per-size / per-quality image tiers are separate catalog model entries
// (dall-e-3 vs dall-e-3-hd), not a pricing-schema extension.
//
// Dispatch rule inside each formula: provider-reported usage tokens win
// when present (they are authoritative for token-priced models); the
// modality unit is the fallback. Zero units + zero tokens → zero cost; the
// stamping site (not the formula) is responsible for warning on
// underivable units.

// costForInputUnits prices N modality units against InputUsdPerM (per
// million units). Modality output is binary (image/audio artifacts), never
// token-priced here, so only the input component is populated.
func costForInputUnits(units float64, prices metrics.ModelPrices) metrics.Cost {
	if prices.InputUsdPerM == nil || units <= 0 {
		return metrics.Cost{}
	}
	usd := units / 1e6 * (*prices.InputUsdPerM)
	return metrics.Cost{UncachedInput: usd, Total: usd}
}

// imageCostFormula prices image-generation requests. Token-usage image
// models (gpt-image-1 reports prompt/completion usage) price via the token
// path; per-image models (dall-e-*) price Images × per-image rate.
func imageCostFormula(u BillableUnits, prices metrics.ModelPrices) metrics.Cost {
	if u.PromptTokens > 0 || u.CompletionTokens > 0 {
		return costForTokens(u.PromptTokens, u.CompletionTokens, prices)
	}
	return costForInputUnits(float64(u.Images), prices)
}

// ttsCostFormula prices text-to-speech requests. Token-usage TTS models
// price via the token path; per-character models (tts-1) price
// InputChars × per-character rate.
func ttsCostFormula(u BillableUnits, prices metrics.ModelPrices) metrics.Cost {
	if u.PromptTokens > 0 || u.CompletionTokens > 0 {
		return costForTokens(u.PromptTokens, u.CompletionTokens, prices)
	}
	return costForInputUnits(float64(u.InputChars), prices)
}

// sttCostFormula prices speech-to-text requests. Token-usage transcribe
// models price via the token path; per-second models (whisper-1 reports
// `duration`) price AudioSeconds × per-second rate.
func sttCostFormula(u BillableUnits, prices metrics.ModelPrices) metrics.Cost {
	if u.PromptTokens > 0 || u.CompletionTokens > 0 {
		return costForTokens(u.PromptTokens, u.CompletionTokens, prices)
	}
	return costForInputUnits(u.AudioSeconds, prices)
}

// videoCostFormula prices video-generation requests. Token-usage video
// models (none shipped today, but the dispatch rule is uniform across
// modality formulas) price via the token path; per-second models (sora-2,
// Veo) price VideoSeconds × per-second rate, where InputUsdPerM is USD per
// 1M seconds of generated video (sora-2 $0.10/s → 100000).
func videoCostFormula(u BillableUnits, prices metrics.ModelPrices) metrics.Cost {
	if u.PromptTokens > 0 || u.CompletionTokens > 0 {
		return costForTokens(u.PromptTokens, u.CompletionTokens, prices)
	}
	return costForInputUnits(u.VideoSeconds, prices)
}

// rerankCostFormula prices document-reranking requests. Token-priced rerank
// models (Voyage reports usage.total_tokens, stamped as PromptTokens) price
// via the token path; search-unit-priced models (Cohere reports
// meta.billed_units.search_units) price SearchUnits × per-unit rate, where
// InputUsdPerM is USD per 1M search units.
func rerankCostFormula(u BillableUnits, prices metrics.ModelPrices) metrics.Cost {
	if u.PromptTokens > 0 || u.CompletionTokens > 0 {
		return costForTokens(u.PromptTokens, u.CompletionTokens, prices)
	}
	return costForInputUnits(float64(u.SearchUnits), prices)
}

// realtimeCostFormula prices one realtime response (one response.done). Six
// components bill simultaneously; per-endpoint unit semantics: PromptTokens =
// UNCACHED text input, CompletionTokens = text output, plus the four realtime
// split fields. Rate fallbacks mirror the shipped column contracts:
//   - cached TEXT read: nil CachedInputReadUsdPerM → InputUsdPerM ("no
//     discount"), the existing text-cache semantics.
//   - cached AUDIO read: nil CachedAudioInputReadUsdPerM → AudioInputUsdPerM.
//   - PRIMARY audio rates have NO fallback: nil prices that component $0.
//     The relay refuses un-priced models at upgrade under an enforced cost
//     quota (non-nil AND > 0 completeness check), so the $0 arm is reachable
//     only for quota-un-enforced VKs — a spend-visibility gap the stamping
//     site WARNs about (deduped), never a quota bypass.
//
// Components map into the Cost breakdown as UncachedInput (text-in +
// audio-in), CacheRead (cached text + cached audio), Output (text-out +
// audio-out).
func realtimeCostFormula(u BillableUnits, prices metrics.ModelPrices) metrics.Cost {
	const million = 1_000_000.0
	textIn := derefRate(prices.InputUsdPerM, 0)
	textOut := derefRate(prices.OutputUsdPerM, 0)
	cachedText := derefRate(prices.CachedInputReadUsdPerM, textIn)
	audioIn := derefRate(prices.AudioInputUsdPerM, 0)
	audioOut := derefRate(prices.AudioOutputUsdPerM, 0)
	cachedAudio := derefRate(prices.CachedAudioInputReadUsdPerM, audioIn)

	out := metrics.Cost{
		UncachedInput: (float64(u.PromptTokens)*textIn + float64(u.AudioInputTokens)*audioIn) / million,
		CacheRead:     (float64(u.CachedTextReadTokens)*cachedText + float64(u.CachedAudioReadTokens)*cachedAudio) / million,
		Output:        (float64(u.CompletionTokens)*textOut + float64(u.AudioOutputTokens)*audioOut) / million,
	}
	out.Total = out.UncachedInput + out.CacheRead + out.Output
	return out
}

// derefRate returns *p or the fallback when p is nil. Local sibling of
// metrics.derefPrice (unexported there); kept here so the realtime formula's
// fallback chain reads at a glance.
func derefRate(p *float64, fallback float64) float64 {
	if p == nil {
		return fallback
	}
	return *p
}
