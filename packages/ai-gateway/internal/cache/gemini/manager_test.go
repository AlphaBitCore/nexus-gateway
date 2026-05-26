package geminicache

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// stubResolver returns a fixed apiKey and baseURL, or an error if errMsg is set.
type stubResolver struct {
	apiKey  string
	baseURL string
	errMsg  string
}

func (s stubResolver) Resolve(_ context.Context, _, _ string) (string, string, error) {
	if s.errMsg != "" {
		return "", "", &resolveError{s.errMsg}
	}
	return s.apiKey, s.baseURL, nil
}

type resolveError struct{ msg string }

func (e *resolveError) Error() string { return e.msg }

func newTestManager(cfg Config) *Manager {
	return New(nil, stubResolver{apiKey: "key", baseURL: "https://example.com"}, NewMetrics(prometheus.NewRegistry()), cfg, nil)
}

func TestContentHash_Stable(t *testing.T) {
	h1 := contentHash("prov1", "gemini-2.0-flash", `{"parts":[{"text":"hi"}]}`)
	h2 := contentHash("prov1", "gemini-2.0-flash", `{"parts":[{"text":"hi"}]}`)
	if h1 != h2 {
		t.Fatalf("hash not stable: %q != %q", h1, h2)
	}
	if len(h1) != len(redisKeyPrefix)+64 {
		t.Fatalf("unexpected key length: %d", len(h1))
	}
}

func TestContentHash_DiffersByInput(t *testing.T) {
	cases := []struct{ prov, model, sys string }{
		{"p1", "m1", `{"parts":[{"text":"A"}]}`},
		{"p2", "m1", `{"parts":[{"text":"A"}]}`},
		{"p1", "m2", `{"parts":[{"text":"A"}]}`},
		{"p1", "m1", `{"parts":[{"text":"B"}]}`},
	}
	keys := make(map[string]bool)
	for _, c := range cases {
		k := contentHash(c.prov, c.model, c.sys)
		if keys[k] {
			t.Fatalf("hash collision for %+v", c)
		}
		keys[k] = true
	}
}

func TestInject_Disabled(t *testing.T) {
	m := newTestManager(Config{Enabled: false})
	body := []byte(`{"systemInstruction":{"parts":[{"text":"hello"}]},"contents":[]}`)
	out, res, err := m.Inject(context.Background(), "p1", "gemini-2.0-flash", body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Injected {
		t.Fatal("expected no injection when disabled")
	}
	if string(out) != string(body) {
		t.Fatal("body should be unchanged when disabled")
	}
}

func TestInject_NoSystemInstruction(t *testing.T) {
	m := newTestManager(Config{Enabled: true, MinSystemChars: 10})
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	out, res, err := m.Inject(context.Background(), "p1", "gemini-2.0-flash", body)
	if err != nil || res.Injected || string(out) != string(body) {
		t.Fatalf("expected pass-through for body without systemInstruction: injected=%v err=%v", res.Injected, err)
	}
}

func TestInject_BelowThreshold(t *testing.T) {
	m := newTestManager(Config{Enabled: true, MinSystemChars: 10000})
	body := []byte(`{"systemInstruction":{"parts":[{"text":"short"}]},"contents":[]}`)
	out, res, _ := m.Inject(context.Background(), "p1", "m", body)
	if res.Injected || string(out) != string(body) {
		t.Fatal("expected pass-through for short system instruction")
	}
}

func TestInject_CacheHit_Rewrites(t *testing.T) {
	systemJSON := `{"parts":[{"text":"very long system"}]}`
	body := []byte(`{"systemInstruction":` + systemJSON + `,"contents":[{"role":"user","parts":[{"text":"q"}]}]}`)

	// Test the rewriteBody path directly (no Redis needed for this assertion).
	rewritten, err := rewriteBody(body, "cachedContents/abc123")
	if err != nil {
		t.Fatalf("rewriteBody error: %v", err)
	}
	// systemInstruction must be gone.
	if gjsonGetStr(rewritten, "systemInstruction") != "" {
		t.Error("systemInstruction should be removed")
	}
	// cachedContent must be set.
	if gjsonGetStr(rewritten, "cachedContent") != "cachedContents/abc123" {
		t.Errorf("cachedContent not set: %s", rewritten)
	}
	// contents must be preserved.
	if gjsonGetStr(rewritten, "contents.0.role") != "user" {
		t.Errorf("contents not preserved: %s", rewritten)
	}
}

func TestInject_RedisMiss_FiresAsync(t *testing.T) {
	m := newTestManager(Config{Enabled: true, MinSystemChars: 1})
	body := []byte(`{"systemInstruction":{"parts":[{"text":"x"}]},"contents":[]}`)
	// rdb is nil so every lookup is a miss; async goroutine fires (no-op in test).
	out, res, err := m.Inject(context.Background(), "p1", "m", body)
	if err != nil || res.Injected || string(out) != string(body) {
		t.Fatalf("expected pass-through on miss: injected=%v err=%v", res.Injected, err)
	}
}

func TestCircuitBreaker_Opens(t *testing.T) {
	cfg := Config{
		Enabled:                 true,
		MinSystemChars:          1,
		CircuitBreakerThreshold: 3,
		CircuitBreakerOpenSecs:  60,
	}
	m := newTestManager(cfg)

	// Trip the circuit breaker manually.
	for range 3 {
		m.recordFailure(cfg)
	}
	if m.cbOpenUntil.Load() == 0 {
		t.Fatal("circuit breaker should be open after threshold failures")
	}
	// Reset.
	m.resetCircuitBreaker()
	if m.cbOpenUntil.Load() != 0 {
		t.Fatal("circuit breaker should be closed after reset")
	}
}

func TestReload(t *testing.T) {
	m := newTestManager(Config{Enabled: false})
	m.Reload(Config{Enabled: true, MinSystemChars: 9999})
	cfg := m.cfg.get()
	if !cfg.Enabled || cfg.MinSystemChars != 9999 {
		t.Fatal("Reload did not update config")
	}
}

// gjsonGetStr is a minimal helper for test assertions.
func gjsonGetStr(data []byte, path string) string {
	// Use simple json unmarshalling to avoid importing gjson in tests.
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		return ""
	}
	parts := splitPath(path)
	var cur any = v
	for _, p := range parts {
		switch t := cur.(type) {
		case map[string]any:
			cur = t[p]
		case []any:
			// numeric index
			idx := 0
			for _, c := range p {
				idx = idx*10 + int(c-'0')
			}
			if idx < len(t) {
				cur = t[idx]
			} else {
				return ""
			}
		default:
			return ""
		}
	}
	if s, ok := cur.(string); ok {
		return s
	}
	return ""
}

func splitPath(path string) []string {
	var parts []string
	var cur []byte
	for _, c := range path {
		if c == '.' {
			parts = append(parts, string(cur))
			cur = nil
		} else {
			cur = append(cur, byte(c))
		}
	}
	parts = append(parts, string(cur))
	return parts
}
