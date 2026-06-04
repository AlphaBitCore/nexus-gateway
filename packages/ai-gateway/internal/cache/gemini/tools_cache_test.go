package geminicache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRewriteBody_StripsToolFieldsForCacheHit guards the fix for the Gemini 400
// "CachedContent can not be used with GenerateContent request setting
// system_instruction, tools or tool_config": on a cache hit a tool-calling
// request must have systemInstruction AND tools AND toolConfig removed (they live
// in the cachedContent), while the per-turn contents are preserved.
func TestRewriteBody_StripsToolFieldsForCacheHit(t *testing.T) {
	body := []byte(`{"systemInstruction":{"parts":[{"text":"sys"}]},` +
		`"tools":[{"functionDeclarations":[{"name":"f"}]}],` +
		`"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}},` +
		`"contents":[{"role":"user","parts":[{"text":"q"}]}]}`)

	out, err := rewriteBody(body, "cachedContents/x")
	if err != nil {
		t.Fatalf("rewriteBody: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("rewritten body is not valid JSON: %v", err)
	}
	for _, forbidden := range []string{"systemInstruction", "tools", "toolConfig"} {
		if _, present := v[forbidden]; present {
			t.Errorf("%q must be stripped alongside cachedContent (else Gemini 400): %s", forbidden, out)
		}
	}
	if v["cachedContent"] != "cachedContents/x" {
		t.Errorf("cachedContent must be set, got %v", v["cachedContent"])
	}
	if _, ok := v["contents"]; !ok {
		t.Error("per-turn contents must be preserved (only the cached prefix is removed)")
	}
}

// TestContentHash_KeyedOnTools verifies tools / toolConfig participate in the
// cache key (they are folded into the cachedContent, so distinct tool sets need
// distinct cache objects) while preserving the legacy system-only key when there
// are no tools, so existing Redis entries keep hitting.
func TestContentHash_KeyedOnTools(t *testing.T) {
	const sys = `{"parts":[{"text":"sys"}]}`
	const toolsA = `[{"functionDeclarations":[{"name":"a"}]}]`
	const toolsB = `[{"functionDeclarations":[{"name":"b"}]}]`
	const cfgAuto = `{"functionCallingConfig":{"mode":"AUTO"}}`

	if contentHash("p", "m", sys, toolsA, "") == contentHash("p", "m", sys, toolsB, "") {
		t.Error("different tool sets under the same system must hash to different cache keys")
	}
	if contentHash("p", "m", sys, "", "") == contentHash("p", "m", sys, toolsA, "") {
		t.Error("a no-tool request must not share a cache key with a tool-bearing one")
	}
	if contentHash("p", "m", sys, toolsA, "") == contentHash("p", "m", sys, toolsA, cfgAuto) {
		t.Error("toolConfig must also participate in the key")
	}

	// Backward compatibility: with no tools/toolConfig the key is byte-identical to
	// the historical system-only hash, so cache entries created before this change
	// keep hitting for non-tool requests.
	legacy := sha256.Sum256([]byte("p|m|" + canonicalizeJSON(sys)))
	want := redisKeyPrefix + hex.EncodeToString(legacy[:])
	if got := contentHash("p", "m", sys, "", ""); got != want {
		t.Errorf("empty-tools key must equal the legacy system-only key\n got=%s\nwant=%s", got, want)
	}
}

// TestAPIClient_Create_FoldsTools asserts the create payload carries the tools
// and toolConfig blocks so they live inside the cachedContent.
func TestAPIClient_Create_FoldsTools(t *testing.T) {
	var seen map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &seen)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"cachedContents/t","expireTime":"2026-05-17T00:00:00Z","usageMetadata":{"totalTokenCount":7}}`))
	}))
	defer srv.Close()

	c := newAPIClient()
	_, err := c.create(context.Background(), "k", srv.URL, "gemini-2.0-flash",
		`{"parts":[{"text":"x"}]}`,
		`[{"functionDeclarations":[{"name":"f"}]}]`,
		`{"functionCallingConfig":{"mode":"AUTO"}}`, 600)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, ok := seen["tools"]; !ok {
		t.Errorf("create payload must carry tools; got %v", seen)
	}
	if _, ok := seen["toolConfig"]; !ok {
		t.Errorf("create payload must carry toolConfig; got %v", seen)
	}
}

// TestAPIClient_Create_InvalidToolJSON covers the two new validation branches:
// malformed tools / toolConfig JSON must surface an error, never a bad payload.
func TestAPIClient_Create_InvalidToolJSON(t *testing.T) {
	c := newAPIClient()
	if _, err := c.create(context.Background(), "k", "https://example.invalid", "m",
		`{"parts":[]}`, `{not json`, "", 60); err == nil {
		t.Error("invalid tools JSON should error before any request")
	}
	if _, err := c.create(context.Background(), "k", "https://example.invalid", "m",
		`{"parts":[]}`, "", `{not json`, 60); err == nil {
		t.Error("invalid toolConfig JSON should error before any request")
	}
}

// TestInject_ToolBearingMiss_PassesThrough exercises the tool-extraction path in
// Inject (rawIfPresent for both tools and toolConfig). With a nil Redis client the
// lookup misses, so the original body passes through untouched — the rewrite that
// strips the tool fields only happens on a hit (covered by rewriteBody tests).
func TestInject_ToolBearingMiss_PassesThrough(t *testing.T) {
	m := newTestManager(Config{Enabled: true, MinSystemChars: 1})
	body := []byte(`{"systemInstruction":{"parts":[{"text":"big system"}]},` +
		`"tools":[{"functionDeclarations":[{"name":"f"}]}],` +
		`"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}},"contents":[]}`)
	out, res, err := m.Inject(context.Background(), "p1", "m", body)
	if err != nil || res.Injected || string(out) != string(body) {
		t.Fatalf("tool-bearing miss should pass through unchanged: injected=%v err=%v", res.Injected, err)
	}
}
