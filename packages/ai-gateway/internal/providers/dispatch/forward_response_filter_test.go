package dispatch

import (
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
)

// TestFilterResponseHeaders_LiveVsCacheHit verifies that live responses
// (isCacheHit=false) forward both static + per-request headers, while
// cache hits (isCacheHit=true) forward only static headers.
func TestFilterResponseHeaders_LiveVsCacheHit(t *testing.T) {
	allowlist := forwardheader.Default()
	src := http.Header{
		"Openai-Version":               []string{"2024-02-15"},
		"X-Request-Id":                 []string{"req-abc"},
		"Openai-Processing-Ms":         []string{"137"},
		"X-Ratelimit-Remaining-Tokens": []string{"42000"},
	}

	live := FilterResponseHeaders(allowlist, FormatOpenAI, src, false)
	for _, h := range []string{"Openai-Version", "X-Request-Id", "Openai-Processing-Ms", "X-Ratelimit-Remaining-Tokens"} {
		if live.Get(h) == "" {
			t.Errorf("live MISS dropped %q", h)
		}
	}

	hit := FilterResponseHeaders(allowlist, FormatOpenAI, src, true)
	if hit.Get("Openai-Version") == "" {
		t.Error("cache HIT dropped static openai-version")
	}
	for _, perReq := range []string{"X-Request-Id", "Openai-Processing-Ms", "X-Ratelimit-Remaining-Tokens"} {
		if hit.Get(perReq) != "" {
			t.Errorf("cache HIT did not strip per-request %q", perReq)
		}
	}
}

// TestFilterResponseHeaders_PerAdapterIsolation verifies that a header
// allowed for openai is not forwarded for anthropic (and vice-versa).
func TestFilterResponseHeaders_PerAdapterIsolation(t *testing.T) {
	allowlist := forwardheader.Default()
	src := http.Header{
		// upstream emitting openai-version on an anthropic call
		// (adversarial / misconfigured upstream)
		"Openai-Version": []string{"2024-02-15"},
	}
	got := FilterResponseHeaders(allowlist, FormatAnthropic, src, false)
	if v := got.Get("Openai-Version"); v != "" {
		t.Errorf("anthropic leaked openai-version=%q", v)
	}
}

// TestFilterResponseHeaders_DenylistedAlwaysDropped verifies that even
// when an upstream emits a denylisted header (set-cookie, server, via,
// cf-ray, …), it never reaches the client output.
func TestFilterResponseHeaders_DenylistedAlwaysDropped(t *testing.T) {
	allowlist := forwardheader.Default()
	src := http.Header{
		"Set-Cookie":                  []string{"sid=evil"},
		"Server":                      []string{"upstream-secret"},
		"Via":                         []string{"upstream-cdn"},
		"Cf-Ray":                      []string{"abc123"},
		"Access-Control-Allow-Origin": []string{"*"},
	}
	got := FilterResponseHeaders(allowlist, FormatOpenAI, src, false)
	for k := range src {
		if got.Get(k) != "" {
			t.Errorf("denylisted header %q leaked through", k)
		}
	}
}

// TestFilterResponseHeaders_NilAllowlistFallsBackToDefault confirms
// that a nil resolved allowlist quietly falls back to the embedded
// defaults — the contract NewSpecAdapter callers (tests) rely on.
func TestFilterResponseHeaders_NilAllowlistFallsBackToDefault(t *testing.T) {
	src := http.Header{"Openai-Version": []string{"2024-02-15"}}
	got := FilterResponseHeaders(nil, FormatOpenAI, src, false)
	if got.Get("Openai-Version") == "" {
		t.Error("nil allowlist did not fall back to embedded defaults")
	}
}

// TestForwardHeaders_DropsAuthorizationFromClient confirms that
// even when something tries to put authorization on the
// request side (which the YAML validator rejects, but a hostile
// client may still send), forwardHeaders drops it before
// Transport.ApplyAuth re-applies the upstream credential.
func TestForwardHeaders_DropsAuthorizationFromClient(t *testing.T) {
	tr := &noopTransport{}
	a := &specAdapter{
		spec: AdapterSpec{
			Format:          FormatOpenAI,
			Transport:       tr,
			SchemaCodec:     noopCodec{},
			StreamDecoder:   noopStream{},
			ErrorNormalizer: noopNorm{},
		},
	}
	dst, _ := http.NewRequest(http.MethodPost, "http://example.invalid", nil)
	src := http.Header{
		"Authorization": []string{"Bearer client-token-do-not-leak"},
		"Content-Type":  []string{"application/json"},
	}
	a.forwardHeaders(dst, src)
	if v := dst.Header.Get("Authorization"); v != "" {
		t.Errorf("Authorization leaked: %q", v)
	}
	if v := dst.Header.Get("Content-Type"); v != "application/json" {
		t.Errorf("Content-Type missing: %q", v)
	}
}
