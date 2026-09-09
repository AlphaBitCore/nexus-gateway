package specutil_test

import (
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/wireformat"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/codec"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
)

// The two bodies below are the same request in the two spellings Google's JSON
// surface accepts. Every reader in the gateway must give them the same answer.
//
// This test exists because "fix the class" was claimed once and delivered
// three-sevenths of the way: three readers were converted and four were not,
// and the four were found by a reviewer rather than by anything executable.
// A per-reader test would have had the same gap — it only covers the readers
// someone remembered. This one covers a PROPERTY, so a new reader that forgets
// the spelling is a new failing case here rather than a silent drop in prod.
const (
	camelRequest = `{
		"contents": [{"role": "user", "parts": [{"text": "hi"}]}],
		"systemInstruction": {"parts": [{"text": "ANSWER ONLY IN FRENCH"}]},
		"generationConfig": {"temperature": 0.1, "responseSchema": {"type": "object"}},
		"safetySettings": [{"category": "HARM_CATEGORY_HARASSMENT", "threshold": "BLOCK_NONE"}],
		"toolConfig": {"functionCallingConfig": {"mode": "AUTO"}},
		"tools": [{"functionDeclarations": [{"name": "get_weather"}]}]
	}`
	snakeRequest = `{
		"contents": [{"role": "user", "parts": [{"text": "hi"}]}],
		"system_instruction": {"parts": [{"text": "ANSWER ONLY IN FRENCH"}]},
		"generation_config": {"temperature": 0.1, "responseSchema": {"type": "object"}},
		"safety_settings": [{"category": "HARM_CATEGORY_HARASSMENT", "threshold": "BLOCK_NONE"}],
		"tool_config": {"functionCallingConfig": {"mode": "AUTO"}},
		"tools": [{"functionDeclarations": [{"name": "get_weather"}]}]
	}`
)

// TestGeminiSpellings_EveryReaderAgrees walks the readers that decide something
// about a Gemini request and asserts each answers identically for both
// spellings, with a positive control on the camelCase side so "agrees" can
// never be satisfied by both readers finding nothing.
func TestGeminiSpellings_EveryReaderAgrees(t *testing.T) {
	t.Run("the cachedContent extractor", func(t *testing.T) {
		// Gemini's implicit prompt cache is GATED on this field's presence, so a
		// reader that misses it means the cache silently never applies and every
		// repeat request pays full price.
		camelSys, camelTools, camelTC := codec.ExtractCacheableFields([]byte(camelRequest))
		snakeSys, snakeTools, snakeTC := codec.ExtractCacheableFields([]byte(snakeRequest))

		if camelSys == "" {
			t.Fatal("the camelCase control found no system instruction — this test cannot see the field it guards")
		}
		if snakeSys != camelSys {
			t.Errorf("systemInstruction disagrees:\n camel: %s\n snake: %s", camelSys, snakeSys)
		}
		if snakeTools != camelTools {
			t.Errorf("tools disagrees:\n camel: %s\n snake: %s", camelTools, snakeTools)
		}
		if camelTC == "" {
			t.Fatal("the camelCase control found no toolConfig")
		}
		if snakeTC != camelTC {
			t.Errorf("toolConfig disagrees:\n camel: %s\n snake: %s", camelTC, snakeTC)
		}
	})

	t.Run("the request-shape validator", func(t *testing.T) {
		// A validator that only understands camelCase waves a malformed
		// protobuf-spelled body through to the upstream.
		if err := wireformat.ValidateGeminiGenerateContentRequest([]byte(camelRequest)); err != nil {
			t.Fatalf("the camelCase control did not validate: %v", err)
		}
		if err := wireformat.ValidateGeminiGenerateContentRequest([]byte(snakeRequest)); err != nil {
			t.Errorf("a well-formed protobuf-spelled request was rejected: %v", err)
		}

		// And the control that proves the validator is looking: a malformed
		// generation_config must be REFUSED, not waved through.
		bad := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generation_config":"not an object"}`
		if err := wireformat.ValidateGeminiGenerateContentRequest([]byte(bad)); err == nil {
			t.Error("a malformed generation_config passed validation — the shape check does not see this spelling")
		}
	})

	t.Run("the shared path helpers", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			paths []string
			probe string
		}{
			{"system instruction", specutil.GeminiSystemInstructionPaths, "0.text"},
			{"generation config", specutil.GeminiGenerationConfigPaths, "temperature"},
			{"safety settings", specutil.GeminiSafetySettingsPaths, "0.category"},
			{"tool config", specutil.GeminiToolConfigPaths, "functionCallingConfig.mode"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				camel := firstOf(camelRequest, tc.paths).Get(tc.probe).String()
				snake := firstOf(snakeRequest, tc.paths).Get(tc.probe).String()
				if camel == "" {
					t.Fatalf("the camelCase control found nothing at %q — the probe cannot see this field", tc.probe)
				}
				if snake != camel {
					t.Errorf("disagreement: camel=%q snake=%q", camel, snake)
				}
			})
		}
	})
}

func firstOf(body string, paths []string) gjson.Result {
	root := gjson.Parse(body)
	for _, p := range paths {
		if v := root.Get(p); v.Exists() {
			return v
		}
	}
	return gjson.Result{}
}
