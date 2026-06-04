package codecs

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"math"
	"testing"
)

// TestScoreTier1Confidence_RatioRubric exercises the boundary anchor
// points the rubric promises: clean full parse = 1.00, one required
// missing = ~0.867, two required missing = ~0.733, all required
// missing = 0.60 (sub-threshold so Coordinator falls through), and
// the unknown-field penalty is bounded at −0.10.
func TestScoreTier1Confidence_RatioRubric(t *testing.T) {
	spec := core.FieldSpec{
		Required: []string{"model", "choices", "usage"},
		Optional: []string{"id", "object", "created", "system_fingerprint", "service_tier"},
	}

	cases := []struct {
		name    string
		body    string
		wantMin float64
		wantMax float64
	}{
		{
			name:    "full parse, no unknowns",
			body:    `{"model":"gpt-4o","choices":[],"usage":{},"id":"x","object":"chat.completion","created":1,"system_fingerprint":"fp","service_tier":"default"}`,
			wantMin: 0.999,
			wantMax: 1.001,
		},
		{
			name: "one required missing (no usage)",
			// 2/3 required + 5 optional → cap +0.10 + 0 unknowns
			// 0.50 + 0.267 + 0.10 = 0.867
			body:    `{"model":"gpt-4o","choices":[],"id":"x","object":"chat.completion","created":1,"system_fingerprint":"fp","service_tier":"default"}`,
			wantMin: 0.866,
			wantMax: 0.868,
		},
		{
			name: "two required missing (no choices, no usage)",
			// 1/3 required + 5 optional → cap +0.10 + 0 unknowns
			// 0.50 + 0.133 + 0.10 = 0.733
			body:    `{"model":"gpt-4o","id":"x","object":"chat.completion","created":1,"system_fingerprint":"fp","service_tier":"default"}`,
			wantMin: 0.732,
			wantMax: 0.734,
		},
		{
			name: "all required missing — sub-threshold, triggers Tier-2 fallthrough",
			// 0/3 required + 5 optional → cap +0.10 + 0 unknowns = 0.60
			body:    `{"id":"x","object":"chat.completion","created":1,"system_fingerprint":"fp","service_tier":"default"}`,
			wantMin: 0.599,
			wantMax: 0.601,
		},
		{
			name: "full + 3 unknown of 11 observed (spec drift signal)",
			// 3/3 req + 5 optional → cap +0.10 − 0.10*(3/11) ≈ 1.00 − 0.0273
			body:    `{"model":"x","choices":[],"usage":{},"id":"x","object":"chat.completion","created":1,"system_fingerprint":"fp","service_tier":"default","brand_new_field":1,"another_drift":2,"third_unknown":3}`,
			wantMin: 0.971,
			wantMax: 0.974,
		},
		{
			name: "all-unknown body (catastrophic drift) clamps to floor",
			// 0/3 req + 0 optional + (−0.10 unknown) = 0.40 floor
			body:    `{"completely":"different","wire":"format","that":1}`,
			wantMin: 0.399,
			wantMax: 0.401,
		},
		{
			name: "sparse-optional response (3 of many declared optional observed)",
			// 3/3 req + 3 optional × 0.025 = +0.075 + 0 unknowns = 0.975
			body:    `{"model":"x","choices":[],"usage":{},"id":"x","object":"chat.completion","created":1}`,
			wantMin: 0.974,
			wantMax: 0.976,
		},
		{
			name: "empty body falls back to 0.90 baseline",
			body: ``,
			// Empty observed → 0.90 baseline (no ratios to compute).
			wantMin: 0.899,
			wantMax: 0.901,
		},
		{
			name: "non-JSON garbage → 0.90 baseline",
			body: `not even json`,
			// topLevelKeys returns nil → 0.90.
			wantMin: 0.899,
			wantMax: 0.901,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := core.ScoreTier1Confidence([]byte(tc.body), spec)
			if got < tc.wantMin || got > tc.wantMax {
				t.Errorf("score = %.4f, want in [%.4f, %.4f]", got, tc.wantMin, tc.wantMax)
			}
		})
	}
}

// TestScoreTier1Confidence_SSEFirstChunk verifies SSE bodies score
// against the FIRST data chunk's top-level keys, not the raw frame
// envelope. Both OpenAI Chat (`data: {...}`) and Anthropic
// (`event: ...\ndata: {...}`) framing shapes are covered.
func TestScoreTier1Confidence_SSEFirstChunk(t *testing.T) {
	spec := core.FieldSpec{
		Required: []string{"model", "choices"},
		Optional: []string{"id", "object", "created"},
	}

	// OpenAI Chat stream first chunk — has model, choices, id, object,
	// created → 2/2 required + 3 optional × 0.025 = +0.075 + 0 unknowns
	// = 0.975. Below the 4-optional cap, so not 1.00.
	openAIStream := `data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n"
	got := core.ScoreTier1Confidence([]byte(openAIStream), spec)
	if got < 0.974 || got > 0.976 {
		t.Errorf("OpenAI stream score = %.4f, want ~0.975", got)
	}

	// Anthropic-style framing — event: lines stripped, first data line
	// has only `type`+`message` → both unknown vs the openai-chat spec →
	// observed=2, unknown=2 → 0.50 + 0 + 0 − 0.10*1.0 = 0.40 floor.
	anthropicStream := `event: message_start
data: {"type":"message_start","message":{"id":"x"}}

`
	got = core.ScoreTier1Confidence([]byte(anthropicStream), spec)
	if got < 0.399 || got > 0.401 {
		t.Errorf("Anthropic-style stream against openai-chat spec = %.4f, want 0.40 floor", got)
	}

	// [DONE]-only stream → no usable first chunk → empty observed → 0.90.
	doneOnly := "data: [DONE]\n\n"
	got = core.ScoreTier1Confidence([]byte(doneOnly), spec)
	if got < 0.899 || got > 0.901 {
		t.Errorf("[DONE]-only stream = %.4f, want 0.90 baseline", got)
	}
}

// TestScoreTier1Confidence_EmptySpecs covers a normalizer that declares
// neither required nor optional keys. The function should not divide by
// zero — both terms collapse to their full +0.40 / +0.10 contributions
// and the score sits at the post-unknown-penalty equivalent of "we
// know nothing about this protocol but the parse succeeded".
func TestScoreTier1Confidence_EmptySpecs(t *testing.T) {
	spec := core.FieldSpec{} // no Required, no Optional
	body := `{"any":"shape","at":"all"}`
	got := core.ScoreTier1Confidence([]byte(body), spec)
	// 0.50 + 0.40 + 0.10 − 0.10*(3/3) = 0.90 (all keys unknown)
	if math.Abs(got-0.90) > 0.001 {
		t.Errorf("empty-spec score = %.4f, want 0.90", got)
	}
}

// TestScoreTier1Confidence_OptionalBonusCap verifies the per-field
// optional bonus saturates at +0.10 once 4+ optional fields are
// observed — preventing a long optional declaration from suppressing
// the bonus AND preventing a flood of observed optional fields from
// inflating the score past 1.00.
func TestScoreTier1Confidence_OptionalBonusCap(t *testing.T) {
	spec := core.FieldSpec{
		Required: []string{"a"},
		Optional: []string{"o1", "o2", "o3", "o4", "o5", "o6", "o7", "o8"},
	}
	cases := []struct {
		name string
		body string
		want float64
	}{
		{"0 optional observed", `{"a":1}`, 0.90}, // 0.5 + 0.4 + 0 - 0
		{"1 optional observed", `{"a":1,"o1":1}`, 0.925},
		{"2 optional observed", `{"a":1,"o1":1,"o2":1}`, 0.95},
		{"3 optional observed", `{"a":1,"o1":1,"o2":1,"o3":1}`, 0.975},
		{"4 optional observed (cap)", `{"a":1,"o1":1,"o2":1,"o3":1,"o4":1}`, 1.00},
		{"8 optional observed (still cap)", `{"a":1,"o1":1,"o2":1,"o3":1,"o4":1,"o5":1,"o6":1,"o7":1,"o8":1}`, 1.00},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := core.ScoreTier1Confidence([]byte(tc.body), spec)
			if math.Abs(got-tc.want) > 0.001 {
				t.Errorf("score = %.4f, want %.4f", got, tc.want)
			}
		})
	}
}

// TestScoreTier1Confidence_RealProdShapes ensures the per-normalizer
// core.FieldSpec declarations score real prod-shape payloads at the right
// quality tier. Each row corresponds to a known-good response shape
// that was actually observed in prod/local traffic_event_normalized
// verified against real prod traffic_event_normalized shapes — the threshold
// guard catches regression if a future change accidentally moves a
// healthy parse below 0.70 (which would silently fall through to Tier-2).
func TestScoreTier1Confidence_RealProdShapes(t *testing.T) {
	cases := []struct {
		name    string
		spec    core.FieldSpec
		body    string
		wantMin float64
	}{
		{
			name:    "openai-chat full response (prod 96.8% of openai-chat rows)",
			spec:    openAIChatFieldSpec(core.DirectionResponse),
			body:    `{"id":"chatcmpl-x","object":"chat.completion","created":1778900000,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6},"system_fingerprint":"fp_x"}`,
			wantMin: 0.95,
		},
		{
			name:    "anthropic-messages full response",
			spec:    anthropicMessagesFieldSpec(core.DirectionResponse),
			body:    `{"id":"msg_x","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}`,
			wantMin: 0.95,
		},
		{
			name:    "gemini-generate full response",
			spec:    geminiGenerateFieldSpec(core.DirectionResponse),
			body:    `{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6},"modelVersion":"gemini-2.5-flash","responseId":"r-x"}`,
			wantMin: 0.95,
		},
		{
			name:    "openai-responses full response (was Tier-3 fallback pre-Gap2)",
			spec:    openAIResponsesFieldSpec(core.DirectionResponse),
			body:    `{"id":"resp_x","object":"response","created_at":1778900000,"model":"gpt-5","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6}}`,
			wantMin: 0.95,
		},
		{
			name: "openai-chat response WITHOUT usage (prod 3.2% — was 0.920)",
			spec: openAIChatFieldSpec(core.DirectionResponse),
			body: `{"id":"x","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"system_fingerprint":"fp"}`,
			// 2/3 required (no usage) + 4 optional × 0.025 = +0.10 cap + 0 unknowns
			// = 0.50 + 0.267 + 0.10 = 0.867
			wantMin: 0.85, // safely above 0.70 threshold; still Tier-1
		},
		{
			name:    "openai-chat request shape",
			spec:    openAIChatFieldSpec(core.DirectionRequest),
			body:    `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":false,"temperature":0.7,"max_tokens":100}`,
			wantMin: 0.85, // 2/2 req + 3/many optional + 0 unknowns
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := core.ScoreTier1Confidence([]byte(tc.body), tc.spec)
			if got < tc.wantMin {
				t.Errorf("score = %.4f, want >= %.4f for healthy parse", got, tc.wantMin)
			}
			if got < 0.70 {
				t.Errorf("score = %.4f BELOW Coordinator threshold 0.70 — would fall through to Tier-2 silently", got)
			}
		})
	}
}

// TestScoreTier1Confidence_SpecDriftSignal verifies the algorithm
// surfaces real spec drift via lowered confidence — the whole point of
// rewriting from the discrete-bonus rubric to a ratio rubric.
func TestScoreTier1Confidence_SpecDriftSignal(t *testing.T) {
	spec := openAIChatFieldSpec(core.DirectionResponse)

	cleanBody := `{"id":"x","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`
	driftBody := `{"id":"x","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6},"safety_classifications":{},"experiment_id":"e-1","new_provider_field":"x"}`

	clean := core.ScoreTier1Confidence([]byte(cleanBody), spec)
	drift := core.ScoreTier1Confidence([]byte(driftBody), spec)
	if drift >= clean {
		t.Errorf("drift score %.4f should be < clean score %.4f — algorithm not surfacing spec-drift signal", drift, clean)
	}
	if clean-drift > 0.10 {
		t.Errorf("drift score %.4f vs clean %.4f — penalty %.4f exceeds documented −0.10 cap", drift, clean, clean-drift)
	}
}
