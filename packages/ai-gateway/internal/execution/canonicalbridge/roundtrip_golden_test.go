package canonicalbridge

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tidwall/gjson"

	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// canonicalAssertions captures the T-ROUND-TRIP-GOLDEN field set every
// codec must preserve through ingress→canonical. One entry per
// supported ingress format. Each row also names the rich fixture under
// testdata/ that exercises the full set.
type canonicalAssertions struct {
	model             string
	systemSubstr      string  // expected fragment in messages[role=system].content
	temperature       float64 // expected canonical value
	topP              float64
	topK              int
	maxTokens         int
	stopValues        []string // expected stop sequence members (canonical)
	toolNames         []string
	hasResponseFormat bool
}

var richIngressFixtures = []struct {
	ingress  provcore.Format
	fixture  string
	assert   canonicalAssertions
	provider string // canonical model after providerModelID override (== fixture model when "" passed)
}{
	{
		ingress: provcore.FormatOpenAI,
		fixture: "openai_chat_tools.request.json",
		assert: canonicalAssertions{
			model:             "gpt-4o-mini",
			systemSubstr:      "weather assistant",
			temperature:       0.0,
			topP:              0.95,
			topK:              40,
			maxTokens:         256,
			stopValues:        []string{"END", "<|eot|>"},
			toolNames:         []string{"get_weather"},
			hasResponseFormat: true,
		},
	},
	{
		ingress: provcore.FormatAnthropic,
		fixture: "anthropic_chat_tooluse.native.json",
		assert: canonicalAssertions{
			model:        "claude-3-5-sonnet-20241022",
			systemSubstr: "weather assistant",
			temperature:  0.0,
			topP:         0.95,
			topK:         40,
			maxTokens:    256,
			stopValues:   []string{"END", "<|eot|>"},
			toolNames:    []string{"get_weather"},
		},
	},
	{
		ingress: provcore.FormatGemini,
		fixture: "gemini_chat_functioncall.native.json",
		assert: canonicalAssertions{
			model:        "gemini-1.5-flash",
			systemSubstr: "weather assistant",
			temperature:  0.0,
			topP:         0.95,
			topK:         40,
			maxTokens:    256,
			stopValues:   []string{"END", "<|eot|>"},
			toolNames:    []string{"get_weather"},
		},
	},
}

// TestRoundTripGolden_S1_Canonical asserts T-ROUND-TRIP-GOLDEN field
// preservation on the ingress→canonical surface for every supported
// ingress format using vendor-derived fixtures.
func TestRoundTripGolden_S1_Canonical(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(nil))
	for _, row := range richIngressFixtures {
		t.Run(string(row.ingress), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", row.fixture))
			if err != nil {
				t.Fatal(err)
			}
			ct := provcore.CallTarget{Format: row.ingress, ProviderModelID: row.assert.model}
			canon, err := b.IngressChatToCanonical(row.ingress, raw, ct)
			if err != nil {
				t.Fatalf("ingress→canonical: %v", err)
			}
			assertCanonical(t, canon, row.assert)
		})
	}
}

func assertCanonical(t *testing.T, canon []byte, want canonicalAssertions) {
	t.Helper()
	if got := gjson.GetBytes(canon, "model").String(); got != want.model {
		t.Errorf("model=%q want %q", got, want.model)
	}
	if want.systemSubstr != "" {
		systemFound := false
		gjson.GetBytes(canon, "messages").ForEach(func(_, msg gjson.Result) bool {
			if msg.Get("role").String() == "system" && contains(msg.Get("content").String(), want.systemSubstr) {
				systemFound = true
				return false
			}
			return true
		})
		if !systemFound {
			t.Errorf("system message containing %q lost: %s", want.systemSubstr, string(canon))
		}
	}
	if got := gjson.GetBytes(canon, "temperature").Float(); got != want.temperature {
		t.Errorf("temperature=%v want %v", got, want.temperature)
	}
	if got := gjson.GetBytes(canon, "top_p").Float(); got != want.topP {
		t.Errorf("top_p=%v want %v", got, want.topP)
	}
	if got := int(gjson.GetBytes(canon, "top_k").Int()); got != want.topK {
		t.Errorf("top_k=%d want %d", got, want.topK)
	}
	if got := int(gjson.GetBytes(canon, "max_tokens").Int()); got != want.maxTokens {
		t.Errorf("max_tokens=%d want %d", got, want.maxTokens)
	}
	if len(want.stopValues) > 0 {
		stop := gjson.GetBytes(canon, "stop")
		var got []string
		switch {
		case stop.IsArray():
			stop.ForEach(func(_, v gjson.Result) bool {
				got = append(got, v.String())
				return true
			})
		case stop.Type == gjson.String:
			got = []string{stop.String()}
		}
		if !sameSet(got, want.stopValues) {
			t.Errorf("stop=%v want %v", got, want.stopValues)
		}
	}
	if len(want.toolNames) > 0 {
		var got []string
		gjson.GetBytes(canon, "tools").ForEach(func(_, t gjson.Result) bool {
			if name := t.Get("function.name").String(); name != "" {
				got = append(got, name)
			}
			return true
		})
		if !sameSet(got, want.toolNames) {
			t.Errorf("tool names=%v want %v", got, want.toolNames)
		}
	}
	if want.hasResponseFormat && !gjson.GetBytes(canon, "response_format").Exists() {
		t.Errorf("response_format lost: %s", string(canon))
	}
}

// targetLandmark holds the per-target shape assertions that prove
// canonical → wire actually emitted target-format JSON. Each landmark
// is a gjson path that must be non-empty after EncodeRequest. We assert
// shape rather than full equivalence because target wire schemas differ
// from canonical (e.g. Gemini renames messages → contents).
type targetLandmark struct {
	target    provcore.Format
	paths     []string // every path must exist
	maxTokens string   // gjson path that must equal canonical max_tokens
}

var targetLandmarks = []targetLandmark{
	{
		target:    provcore.FormatOpenAI,
		paths:     []string{"model", "messages", "tools.0.function.name"},
		maxTokens: "max_tokens",
	},
	{
		target:    provcore.FormatAnthropic,
		paths:     []string{"model", "messages", "system", "tools.0.name", "tools.0.input_schema"},
		maxTokens: "max_tokens",
	},
	{
		target:    provcore.FormatGemini,
		paths:     []string{"contents", "systemInstruction.parts.0.text", "tools.0.functionDeclarations.0.name", "generationConfig.maxOutputTokens"},
		maxTokens: "generationConfig.maxOutputTokens",
	},
	{
		target:    provcore.FormatVertex,
		paths:     []string{"contents", "systemInstruction.parts.0.text", "tools.0.functionDeclarations.0.name", "generationConfig.maxOutputTokens"},
		maxTokens: "generationConfig.maxOutputTokens",
	},
	{
		target:    provcore.FormatBedrock,
		paths:     []string{"anthropic_version", "messages"},
		maxTokens: "max_tokens",
	},
}

// TestRoundTripGolden_S2_TargetWire asserts T-ROUND-TRIP-GOLDEN
// preservation on the canonical→target wire surface. For each rich
// ingress fixture and each ChatRoutable target, IngressChatToWire must
// produce a body that carries the target's identifying fields and the
// canonical max_tokens value.
func TestRoundTripGolden_S2_TargetWire(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(nil))
	for _, row := range richIngressFixtures {
		raw, err := os.ReadFile(filepath.Join("testdata", row.fixture))
		if err != nil {
			t.Fatal(err)
		}
		for _, lm := range targetLandmarks {
			if !b.ChatRoutable(row.ingress, lm.target) {
				continue
			}
			t.Run(string(row.ingress)+"_to_"+string(lm.target), func(t *testing.T) {
				ct := provcore.CallTarget{Format: lm.target, ProviderModelID: FixtureProviderModel(lm.target)}
				wire, err := b.IngressChatToWire(row.ingress, lm.target, raw, ct)
				if err != nil {
					t.Fatalf("IngressChatToWire(%s→%s): %v", row.ingress, lm.target, err)
				}
				for _, p := range lm.paths {
					if !gjson.GetBytes(wire, p).Exists() {
						t.Errorf("landmark %q absent on wire: %s", p, string(wire))
					}
				}
				if got := int(gjson.GetBytes(wire, lm.maxTokens).Int()); got != row.assert.maxTokens {
					t.Errorf("max_tokens at %q=%d want %d (preservation lost)", lm.maxTokens, got, row.assert.maxTokens)
				}
			})
		}
	}
}

// TestRoundTripGolden_openaiToAnthropicPreservesCoreFields keeps the
// minimal-fixture smoke test from the original SDD work; cheap and
// independent of the rich-fixture matrix above.
func TestRoundTripGolden_openaiToAnthropicPreservesCoreFields(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(nil))
	raw, err := os.ReadFile(filepath.Join("testdata", "openai_chat_minimal.request.json"))
	if err != nil {
		t.Fatal(err)
	}
	ct := provcore.CallTarget{
		Format:          provcore.FormatAnthropic,
		ProviderModelID: FixtureProviderModel(provcore.FormatAnthropic),
	}
	wire, err := b.IngressChatToWire(provcore.FormatOpenAI, provcore.FormatAnthropic, raw, ct)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(wire, "messages").String() == "" {
		t.Fatalf("expected messages: %s", string(wire))
	}
	if gjson.GetBytes(wire, "max_tokens").Int() != 32 {
		t.Fatalf("max_tokens lost")
	}
	if gjson.GetBytes(wire, "temperature").Float() != 0.2 {
		t.Fatalf("temperature lost")
	}
}

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
		if m[x] < 0 {
			return false
		}
	}
	return true
}
