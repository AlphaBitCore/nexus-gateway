package geminicache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/goccy/go-json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The tool-stripping wire shape (systemInstruction/tools/toolConfig removed
// alongside a cachedContent reference) now lives in the Gemini codec as
// InjectCachedContentRef and is tested there
// (specs/gemini/codec/cachedcontent_request_test.go). This file keeps the
// manager-side behaviour: that tools/toolConfig participate in the cache key.

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
