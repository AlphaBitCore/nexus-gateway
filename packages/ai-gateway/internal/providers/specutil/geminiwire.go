package specutil

import "github.com/tidwall/gjson"

// geminiwire.go — the two spellings a Gemini request may use, in one place.
//
// Google's JSON surface for generateContent accepts the protobuf field names
// alongside the documented camelCase ones: `system_instruction` for
// `systemInstruction`, `generation_config` for `generationConfig`. Both are
// real; the repo's own normalize codec declares both
// (transport/normalize/codecs/gemini_generate.go carries a
// SystemInstructionSnake field), and the redaction adapter reads both
// (traffic/adapters/api/gemini/slots.go).
//
// The gateway's request path read only camelCase, so a caller using the
// protobuf spelling had the system prompt and every generation parameter
// silently DROPPED on the way to a non-Gemini target: the model answered with
// no instruction, at default temperature, with no output cap, and the
// estimator under-counted the prompt — which then feeds `auto` prompt-size
// routing. Nothing errored; the request simply became a different request.
//
// One list, read by every site, is the point: a per-site `if snake != ""`
// fixes one reader, and the next reader added reintroduces the defect.
// Consolidating the three lists across modules is worth doing and is not this
// file's job; until then this is the gateway's single copy.

// GeminiSystemInstructionPaths are the gjson paths to the system prompt's
// parts array, camelCase first because it is the documented spelling and the
// one nearly every caller sends.
var GeminiSystemInstructionPaths = []string{"systemInstruction.parts", "system_instruction.parts"}

// GeminiGenerationConfigPaths are the gjson paths to the generation-parameter
// object.
var GeminiGenerationConfigPaths = []string{"generationConfig", "generation_config"}

// GeminiSafetySettingsPaths and GeminiToolConfigPaths complete the set. Every
// multi-word key on this wire has both spellings; `tools` and `contents` are
// single words and therefore identical in both, which is why they need no list.
var (
	GeminiSafetySettingsPaths = []string{"safetySettings", "safety_settings"}
	GeminiToolConfigPaths     = []string{"toolConfig", "tool_config"}

	// The system-instruction OBJECT, for callers that pass the field's JSON
	// along verbatim rather than walking into its parts.
	GeminiSystemInstructionObjectPaths = []string{"systemInstruction", "system_instruction"}
)

// GeminiFirstRaw returns the raw JSON of the first of paths that exists and is
// non-empty, or "" when none does. For callers that pass a field's JSON along
// verbatim rather than reading into it.
func GeminiFirstRaw(body []byte, paths []string) string {
	for _, p := range paths {
		if r := gjson.GetBytes(body, p); r.Exists() && r.Raw != "" {
			return r.Raw
		}
	}
	return ""
}

// GeminiFirst returns the first of paths that exists under root, or a
// non-existent Result when none does — so a caller keeps its usual
// `if v.Exists()` shape and gains nothing to forget.
func GeminiFirst(root gjson.Result, paths []string) gjson.Result {
	for _, p := range paths {
		if v := root.Get(p); v.Exists() {
			return v
		}
	}
	return gjson.Result{}
}

// GeminiFirstBytes is GeminiFirst for a raw body, for callers that hold bytes
// rather than a parsed root.
func GeminiFirstBytes(body []byte, paths []string) gjson.Result {
	for _, p := range paths {
		if v := gjson.GetBytes(body, p); v.Exists() {
			return v
		}
	}
	return gjson.Result{}
}
