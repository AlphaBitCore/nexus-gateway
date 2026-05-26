package extract

import (
	"context"
	"encoding/json"
	"strings"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// PatternNormalizer implements normalize.Normalizer as the Tier-2
// fallback in the Coordinator chain. It runs DetectChatShape on
// request-direction bodies and DetectResponseShape on response-direction
// bodies, returning a populated NormalizedPayload when confidence is at
// or above MinConfidence. Below that threshold it returns
// normalize.ErrUnsupported so the Coordinator falls through to Tier 3
// (generic-http verbatim).
//
// Direction is inferred from meta.Direction. When Direction is unset,
// we try BOTH probes and pick the higher-confidence detection — most
// audit envelopes do set Direction so this is a defensive fallback.
//
// MinConfidence defaults to 0.7. Set lower (e.g. 0.5) to be more
// aggressive about claiming non-canonical bodies as chat shapes; higher
// (e.g. 0.85) to leave more rows in Tier 3 verbatim.
type PatternNormalizer struct {
	MinConfidence float64
}

// NewPatternNormalizer returns a PatternNormalizer with the default
// 0.7 threshold.
func NewPatternNormalizer() *PatternNormalizer {
	return &PatternNormalizer{MinConfidence: 0.7}
}

// ChatSpecByID returns the request-side spec with the matching ID, or
// nil when unknown. Used by per-adapter Normalize methods (Tier 1) to
// pick their preferred spec without re-implementing the lookup.
func ChatSpecByID(id string) *ChatSpec {
	for i := range KnownChatSpecs {
		if KnownChatSpecs[i].ID == id {
			return &KnownChatSpecs[i]
		}
	}
	return nil
}

// ChatResponseSpecByID returns the response-side spec with the matching
// ID, or nil when unknown.
func ChatResponseSpecByID(id string) *ChatResponseSpec {
	for i := range KnownResponseSpecs {
		if KnownResponseSpecs[i].ID == id {
			return &KnownResponseSpecs[i]
		}
	}
	return nil
}

// AdapterSpecHint bundles the spec choices for one per-host adapter.
// Multiple response specs are listed because the same adapter may
// receive both SSE and non-stream responses (Anthropic Messages API,
// OpenAI, Gemini all expose both shapes). ScoreResponseSpec auto-
// filters incompatible framings so passing both is safe.
//
// minConfidence below 0.7 (the Tier-2 threshold) is normal: the
// adapter caller has already routed based on host knowledge, so even
// partial pattern hits are more reliable than a generic multi-spec
// probe.
type AdapterSpecHint struct {
	AdapterID     string
	ReqSpecIDs    []string
	RespSpecIDs   []string
	MinConfidence float64
}

// NormalizeForAdapter is the Tier-1 helper per-adapter Normalizers
// call into. Routes by meta.Direction, scores ALL listed specs for
// that direction, picks the highest-confidence hit, and either
// returns a populated NormalizedPayload (when confidence >=
// hint.MinConfidence) or normalize.ErrUnsupported so the Coordinator
// falls through to Tier 2.
//
// adapterID (taken from hint.AdapterID) is stamped onto DetectedSpec
// without the "pattern:" prefix used by the Tier-2 probe — a
// confirmed per-host parse, not a generic shape match.
func NormalizeForAdapter(raw []byte, meta normalize.Meta, hint AdapterSpecHint) (normalize.NormalizedPayload, error) {
	threshold := hint.MinConfidence
	if threshold <= 0 {
		threshold = 0.5
	}

	scoreReqs := func() ChatDetection {
		var best ChatDetection
		for _, id := range hint.ReqSpecIDs {
			if spec := ChatSpecByID(id); spec != nil {
				if d := ScoreChatSpec(raw, *spec); d.Confidence > best.Confidence {
					best = d
				}
			}
		}
		return best
	}
	scoreResps := func() ChatDetection {
		var best ChatDetection
		for _, id := range hint.RespSpecIDs {
			if spec := ChatResponseSpecByID(id); spec != nil {
				if d := ScoreResponseSpec(raw, *spec); d.Confidence > best.Confidence {
					best = d
				}
			}
		}
		return best
	}

	var d ChatDetection
	switch meta.Direction {
	case normalize.DirectionRequest:
		d = scoreReqs()
	case normalize.DirectionResponse:
		d = scoreResps()
	default:
		// Direction unset: try both, take higher confidence.
		dr := scoreReqs()
		dp := scoreResps()
		if dp.Confidence > dr.Confidence {
			d = dp
		} else {
			d = dr
		}
	}
	if d.Confidence < threshold {
		return normalize.NormalizedPayload{
			Kind:             normalize.KindUnsupported,
			NormalizeVersion: normalize.SchemaVersion,
			Confidence:       d.Confidence,
		}, normalize.ErrUnsupported
	}
	// Adapter caller — stamp adapter ID as the DetectedSpec, no probe prefix.
	d.SpecID = hint.AdapterID
	// AdapterCallerConfidenceFloor: when the caller is an adapter
	// (NormalizeForAdapter is only entered from per-adapter Normalize
	// methods), the AdapterID itself is already a strong signal —
	// the agent / cp / gw chose this adapter because the host / URL /
	// content-type matched. The body-shape score is just confirming
	// "yes, the bytes look like what we expect for this adapter".
	// Without this floor, single-prompt specs (claude-web caps at 0.6
	// via 0.4 ContentPath + 0.2 signature) get rejected by the
	// Registry's default 0.7 threshold even when the spec matched
	// every signature field — confirmed against the live agent capture
	// 2026-05-25 (claude.ai prompt + parent_message_uuid + timezone +
	// locale + personalized_styles + rendering_mode + sync_sources
	// all present → 0.6 → Registry rejected → fell to generic-http).
	if d.Confidence < AdapterCallerConfidenceFloor {
		d.Confidence = AdapterCallerConfidenceFloor
	}
	return BuildPayload(d, raw, ""), nil
}

// AdapterCallerConfidenceFloor is the minimum payload.Confidence that
// NormalizeForAdapter returns once the underlying spec passes the
// adapter's own MinConfidence gate. Set to 0.95 so the Registry's
// default 0.7 threshold always claims the result — adapter resolution
// is the source of truth for "this is adapter X traffic"; body-shape
// scoring is only confirmation, not the primary signal.
const AdapterCallerConfidenceFloor = 0.95

// WireTier2 installs the default PatternNormalizer as Tier 2 of the
// given Registry's Coordinator. Binaries call this once at startup,
// after normalize.RegisterDefaultAIBuiltins, so the Tier-1 adapter
// entries and the Tier-3 *:*:* catch-all are already in place. The
// indirection sidesteps a normalize → extract import cycle (extract
// imports normalize.NormalizedPayload).
func WireTier2(reg *normalize.Registry) {
	reg.RegisterTier2(NewPatternNormalizer())
}

// ID is the metric / log label.
func (p *PatternNormalizer) ID() string { return "pattern-extract" }

// Normalize is the Coordinator Tier-2 entry. Walks two recognizer
// chains and returns the highest-confidence match:
//
//  1. JSON multi-spec probe (DetectChatShape / DetectResponseShape)
//     against KnownChatSpecs + KnownResponseSpecs — claims OpenAI /
//     Anthropic / Gemini / claude-web / completions-legacy shapes.
//  2. NonJSON detector chain — Connect-RPC + protobuf (cursor-like
//     hosts) and Google batchexecute (gemini-web-like hosts). Tier 2
//     recognises these formats even when no per-host adapter is
//     registered, so a new host shipping the same wire shape gets
//     structured Messages automatically.
//
// Returns ErrUnsupported when neither chain scores above
// MinConfidence — the Coordinator then falls through to Tier 3
// verbatim. Returns a populated NormalizedPayload otherwise.
func (p *PatternNormalizer) Normalize(_ context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
	threshold := p.MinConfidence
	if threshold == 0 {
		threshold = 0.7
	}

	dir := meta.Direction

	// Pass 1: JSON multi-spec probe.
	var best ChatDetection
	switch dir {
	case normalize.DirectionRequest:
		best = DetectChatShape(raw)
	case normalize.DirectionResponse:
		best = DetectResponseShape(raw)
	default:
		req := DetectChatShape(raw)
		resp := DetectResponseShape(raw)
		if resp.Confidence > req.Confidence {
			best = resp
		} else {
			best = req
		}
	}

	// Pass 2: non-JSON format detectors (protobuf + batchexecute
	// today; extend NonJSONDetectors to add more shapes). Each
	// detector LooksLike-screens cheaply before doing the full
	// Decode — so a JSON body costs O(N detectors) byte-prefix
	// checks and nothing else.
	for _, d := range NonJSONDetectors {
		if !d.LooksLike(raw) {
			continue
		}
		det, ok := d.Decode(raw, string(dir))
		if !ok {
			continue
		}
		if det.Confidence > best.Confidence {
			best = det
		}
	}

	if best.Confidence < threshold {
		return normalize.NormalizedPayload{
			Kind:             normalize.KindUnsupported,
			NormalizeVersion: normalize.SchemaVersion,
			Confidence:       best.Confidence, // surface partial signal
		}, normalize.ErrUnsupported
	}
	return buildPayload(best, raw), nil
}

// BuildPayload exposes the ChatDetection → NormalizedPayload mapping
// as a public helper so per-adapter Normalizers (Tier 1) can reuse the
// same translation logic the Tier-2 wrapper uses. specPrefix is
// prepended to the detection's SpecID and stamped onto DetectedSpec —
// Tier-2 callers pass "pattern:" while Tier-1 adapter callers pass
// "" (so DetectedSpec just reads e.g. "chatgpt-web", indicating a
// confirmed per-adapter parse).
func BuildPayload(d ChatDetection, raw []byte, specPrefix string) normalize.NormalizedPayload {
	return buildPayloadInternal(d, raw, specPrefix)
}

// buildPayload turns a ChatDetection into a NormalizedPayload. Per
// SDD: keep raw body (BodyView.Text) for dual-view UI, fill all
// extracted fields the spec recognised, and set DetectedSpec /
// Confidence.
func buildPayload(d ChatDetection, raw []byte) normalize.NormalizedPayload {
	return buildPayloadInternal(d, raw, "pattern:")
}

func buildPayloadInternal(d ChatDetection, raw []byte, specPrefix string) normalize.NormalizedPayload {
	protocol := "pattern-extract"
	if specPrefix == "" {
		// Per-adapter caller — Protocol identifies the adapter ID rather
		// than the generic pattern probe.
		protocol = d.SpecID
	}
	out := normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Protocol:         protocol,
		Model:            d.Model,
		Stream:           d.IsStream,
		FinishReason:     d.FinishReason,
		Confidence:       d.Confidence,
		DetectedSpec:     specPrefix + d.SpecID,
		// Dual view: preserve the raw body as a text fallback so the
		// UI's Raw tab can show the original protocol bytes.
		HTTP: &normalize.HTTPPayload{
			BodyView: &normalize.HTTPBodyView{Text: string(raw)},
		},
	}

	// Translate detection's flat Role/Content arrays into typed
	// Message slice. Skip empty content unless role is "system" /
	// "tool" (where empty content carries semantic meaning).
	for i := range d.MessageRoles {
		role := d.MessageRoles[i]
		var content string
		if i < len(d.MessageContents) {
			content = d.MessageContents[i]
		}
		if content == "" && role != "system" && role != "tool" {
			continue
		}
		out.Messages = append(out.Messages, normalize.Message{
			Role: mapRole(role),
			Content: []normalize.ContentBlock{{
				Type: normalize.ContentText,
				Text: content,
			}},
		})
	}

	// Tools — pass through as raw JSON tree under Tools[].Name="raw".
	// Per-spec adapters that implement normalize.Normalizer project
	// Tools into the canonical ToolDef shape directly.
	if len(d.ToolsRaw) > 0 {
		var arr []map[string]any
		if err := json.Unmarshal(d.ToolsRaw, &arr); err == nil {
			for _, t := range arr {
				name, _ := t["name"].(string)
				desc, _ := t["description"].(string)
				params, _ := t["parameters"].(map[string]any)
				out.Tools = append(out.Tools, normalize.ToolDef{
					Name:                 name,
					Description:          desc,
					ParametersJSONSchema: params,
				})
			}
		}
	}

	// Usage — try to map common shapes (OpenAI / Anthropic / Gemini)
	// best-effort. Per text-first memory binding, we don't fabricate
	// numbers; only fill what's parseable.
	if len(d.UsageRaw) > 0 {
		out.Usage = mapUsage(d.UsageRaw)
	}
	return out
}

// mapRole normalises a free-form role string to the normalize.Role
// enum. Unknown roles map to RoleUser as a safe default (so PII hooks
// still scan the content rather than skipping it).
func mapRole(s string) normalize.Role {
	switch strings.ToLower(s) {
	case "system":
		return normalize.RoleSystem
	case "user":
		return normalize.RoleUser
	case "assistant", "model":
		return normalize.RoleAssistant
	case "tool", "function":
		return normalize.RoleTool
	default:
		return normalize.RoleUser
	}
}

// mapUsage decodes a usage object into normalize.Usage. Tries OpenAI's
// `prompt_tokens` / `completion_tokens` keys first, then Anthropic's
// `input_tokens` / `output_tokens`, then Gemini's
// `promptTokenCount` / `candidatesTokenCount`. Returns nil when no
// recognisable keys are present.
func mapUsage(raw json.RawMessage) *normalize.Usage {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	u := &normalize.Usage{}
	any := false
	intPtr := func(key string) *int {
		v, ok := m[key]
		if !ok {
			return nil
		}
		f, ok := v.(float64)
		if !ok {
			return nil
		}
		x := int(f)
		any = true
		return &x
	}
	if p := intPtr("prompt_tokens"); p != nil {
		u.PromptTokens = p
	} else if p := intPtr("input_tokens"); p != nil {
		u.PromptTokens = p
	} else if p := intPtr("promptTokenCount"); p != nil {
		u.PromptTokens = p
	}
	if p := intPtr("completion_tokens"); p != nil {
		u.CompletionTokens = p
	} else if p := intPtr("output_tokens"); p != nil {
		u.CompletionTokens = p
	} else if p := intPtr("candidatesTokenCount"); p != nil {
		u.CompletionTokens = p
	}
	if p := intPtr("total_tokens"); p != nil {
		u.TotalTokens = p
	} else if p := intPtr("totalTokenCount"); p != nil {
		u.TotalTokens = p
	}
	if !any {
		return nil
	}
	return u
}
