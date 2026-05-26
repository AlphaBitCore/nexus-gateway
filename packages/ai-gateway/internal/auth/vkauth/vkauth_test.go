package vkauth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
)

func TestExtractVKToken_Header(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("x-nexus-virtual-key", "my-slug")
	if got := extractVKToken(context.Background(), r); got != "my-slug" {
		t.Errorf("got %q", got)
	}
}

func TestExtractVKToken_Bearer(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer engineering-openai")
	if got := extractVKToken(context.Background(), r); got != "engineering-openai" {
		t.Errorf("got %q", got)
	}
}

func TestExtractVKToken_PreferHeader(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("x-nexus-virtual-key", "from-header")
	r.Header.Set("Authorization", "Bearer from-bearer")
	if got := extractVKToken(context.Background(), r); got != "from-header" {
		t.Errorf("got %q, want from-header", got)
	}
}

func TestExtractVKToken_Missing(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if got := extractVKToken(context.Background(), r); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestLooksLikeRealKey(t *testing.T) {
	tests := []struct {
		token string
		want  bool
	}{
		{"nvk_abc123def456", true},                // starts with nvk_
		{"a-very-long-token-over-20-chars", true}, // length > 20
		{"my-slug", false},                        // short slug
		{"engineering-team", false},               // normal slug
	}
	for _, tt := range tests {
		if got := looksLikeRealKey(tt.token); got != tt.want {
			t.Errorf("looksLikeRealKey(%q) = %v, want %v", tt.token, got, tt.want)
		}
	}
}

func TestClassifyVKToken(t *testing.T) {
	tests := []struct {
		token string
		want  string
	}{
		{"nvk_abc123def456", "nvk_"},
		{"engineering-openai", ""},
		{"", ""},
		{"sk-ant-xxx", ""}, // not a VK
	}
	for _, tt := range tests {
		if got := classifyVKToken(tt.token); got != tt.want {
			t.Errorf("classifyVKToken(%q) = %q, want %q", tt.token, got, tt.want)
		}
	}
}

func TestHashKey_HMAC(t *testing.T) {
	a := &Authenticator{hmacSecret: []byte("test-secret")}
	hash := a.hashKey("nvk_testkey12345678")

	// Verify independently.
	mac := hmac.New(sha256.New, []byte("test-secret"))
	mac.Write([]byte("nvk_testkey12345678"))
	want := hex.EncodeToString(mac.Sum(nil))

	if hash != want {
		t.Errorf("hash mismatch: got %s, want %s", hash, want)
	}

	// Deterministic.
	if a.hashKey("nvk_testkey12345678") != hash {
		t.Error("hash should be deterministic")
	}

	// Different key → different hash.
	if a.hashKey("nvk_otherkey") == hash {
		t.Error("different keys should produce different hashes")
	}
}
