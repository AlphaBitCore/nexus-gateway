package codec

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

// InjectCachedContentRef must strip the three fields Gemini forbids alongside a
// cachedContent reference (systemInstruction, tools, toolConfig — they live in
// the cached object) and set cachedContent, while preserving the per-turn
// contents. Without this a tool-calling request 400s with "CachedContent can not
// be used with GenerateContent request setting system_instruction, tools or
// tool_config".
func TestInjectCachedContentRef_StripsForbiddenFieldsAndSetsRef(t *testing.T) {
	body := []byte(`{"systemInstruction":{"parts":[{"text":"sys"}]},` +
		`"tools":[{"functionDeclarations":[{"name":"f"}]}],` +
		`"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}},` +
		`"contents":[{"role":"user","parts":[{"text":"q"}]}]}`)

	out, err := InjectCachedContentRef(body, "cachedContents/abc123")
	if err != nil {
		t.Fatalf("InjectCachedContentRef: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}
	for _, forbidden := range []string{"systemInstruction", "tools", "toolConfig"} {
		if _, present := v[forbidden]; present {
			t.Errorf("%q must be stripped alongside cachedContent (else Gemini 400): %s", forbidden, out)
		}
	}
	if got, _ := v["cachedContent"].(string); got != "cachedContents/abc123" {
		t.Errorf("cachedContent = %q, want cachedContents/abc123; out=%s", got, out)
	}
	if gjson.GetBytes(out, "contents.0.role").String() != "user" {
		t.Errorf("per-turn contents must be preserved: %s", out)
	}
}

// Deleting an absent field is a no-op: a system-only request (no tools) rewrites
// cleanly, leaving the same shape existing cache entries were created under.
func TestInjectCachedContentRef_SystemOnlyBody(t *testing.T) {
	body := []byte(`{"systemInstruction":{"parts":[{"text":"x"}]},"contents":[{"role":"user","parts":[{"text":"q"}]}]}`)
	out, err := InjectCachedContentRef(body, "cachedContents/xyz")
	if err != nil {
		t.Fatalf("InjectCachedContentRef: %v", err)
	}
	if gjson.GetBytes(out, "systemInstruction").Exists() {
		t.Errorf("systemInstruction should be removed: %s", out)
	}
	if gjson.GetBytes(out, "cachedContent").String() != "cachedContents/xyz" {
		t.Errorf("cachedContent not set: %s", out)
	}
	if gjson.GetBytes(out, "contents.0.role").String() != "user" {
		t.Errorf("contents not preserved: %s", out)
	}
}

// ExtractCacheableFields returns the raw JSON of each cacheable field, and the
// empty string for absent ones — the manager keys its cache on these and skips
// the feature when there is no systemInstruction.
func TestExtractCacheableFields(t *testing.T) {
	full := []byte(`{"systemInstruction":{"parts":[{"text":"s"}]},` +
		`"tools":[{"functionDeclarations":[{"name":"f"}]}],` +
		`"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}},` +
		`"contents":[{"role":"user","parts":[{"text":"q"}]}]}`)
	sys, tools, toolCfg := ExtractCacheableFields(full)
	if sys == "" || tools == "" || toolCfg == "" {
		t.Errorf("all three fields present but got sys=%q tools=%q toolConfig=%q", sys, tools, toolCfg)
	}
	if !gjson.Valid(sys) || gjson.Get(sys, "parts.0.text").String() != "s" {
		t.Errorf("systemInstruction raw JSON malformed: %q", sys)
	}

	// A system-only body: tools/toolConfig come back empty, system populated.
	sysOnly, toolsEmpty, cfgEmpty := ExtractCacheableFields([]byte(`{"systemInstruction":{"parts":[{"text":"s"}]}}`))
	if sysOnly == "" || toolsEmpty != "" || cfgEmpty != "" {
		t.Errorf("system-only: sys=%q tools=%q toolConfig=%q; want tools/toolConfig empty", sysOnly, toolsEmpty, cfgEmpty)
	}

	// No cacheable fields at all → all empty (the manager then skips as no_system).
	s, to, tc := ExtractCacheableFields([]byte(`{"contents":[]}`))
	if s != "" || to != "" || tc != "" {
		t.Errorf("no cacheable fields but got sys=%q tools=%q toolConfig=%q", s, to, tc)
	}
}
