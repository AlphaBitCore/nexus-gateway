package traffic

import (
	"context"
	"net/http"
	"testing"
)

func TestApiKeyFingerprint(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{"empty", "", ""},
		{"short anthropic", "sk-ant-test", "47bd74bd13b3d59f"},
		{"openai project", "sk-proj-abc123xyz", "e21da67e70c8abac"},
		{"virtual key", "nvk_tenant_demo", "81d82dde8e27a74b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ApiKeyFingerprint(tc.key)
			if tc.key == "" {
				if got != "" {
					t.Fatalf("empty key should yield empty fingerprint, got %q", got)
				}
				return
			}
			if len(got) != 16 {
				t.Fatalf("fingerprint must be 16 hex chars, got %q (len=%d)", got, len(got))
			}
			// Stability check: same input always same output.
			if again := ApiKeyFingerprint(tc.key); again != got {
				t.Fatalf("fingerprint not stable: %q vs %q", got, again)
			}
		})
	}
}

func TestApiKeyFingerprintDistinct(t *testing.T) {
	a := ApiKeyFingerprint("sk-ant-alpha")
	b := ApiKeyFingerprint("sk-ant-beta")
	if a == b {
		t.Fatalf("distinct keys must yield distinct fingerprints: %q", a)
	}
}

func TestApiKeyClassify(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"", ""},
		{"sk-ant-abc", "sk-ant-"},
		{"sk-proj-abc", "sk-proj-"},
		{"sk-abcdef", "sk-"},
		{"AIzaSyBsomething", "AIza"},
		{"nvk_something", "nvk_"},
		{"random-opaque-token", ""},
	}
	for _, tc := range tests {
		if got := ApiKeyClassify(tc.key); got != tc.want {
			t.Errorf("ApiKeyClassify(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

func TestExtractBearerToken(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"missing", "", ""},
		{"typical", "Bearer sk-ant-abc", "sk-ant-abc"},
		{"lowercase scheme", "bearer sk-ant-abc", "sk-ant-abc"},
		{"with extra spaces", "Bearer   sk-ant-abc  ", "sk-ant-abc"},
		{"not bearer", "Basic dXNlcjpwYXNz", ""},
		{"empty token", "Bearer ", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.test/x", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			if got := ExtractBearerToken(r); got != tc.want {
				t.Errorf("ExtractBearerToken(%q) = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

func TestExtractBearerTokenNilRequest(t *testing.T) {
	if got := ExtractBearerToken(nil); got != "" {
		t.Fatalf("nil request should return empty, got %q", got)
	}
}
