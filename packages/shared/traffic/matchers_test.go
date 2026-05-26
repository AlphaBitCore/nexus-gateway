package traffic

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/interception"
)

func TestMatchHost_Exact(t *testing.T) {
	tests := []struct {
		host, pattern string
		want          bool
	}{
		{"api.openai.com", "api.openai.com", true},
		{"API.OPENAI.COM", "api.openai.com", true}, // case insensitive
		{"api.anthropic.com", "api.openai.com", false},
	}
	for _, tt := range tests {
		if got := matchHost(tt.host, tt.pattern, interception.HostMatchTypeExact); got != tt.want {
			t.Errorf("matchHost(%q, %q, EXACT) = %v, want %v", tt.host, tt.pattern, got, tt.want)
		}
	}
}

func TestMatchHost_Prefix(t *testing.T) {
	tests := []struct {
		host, pattern string
		want          bool
	}{
		{"api.openai.com", "api.", true},
		{"api.openai.com", "api.openai", true},
		{"www.example.com", "api.", false},
	}
	for _, tt := range tests {
		if got := matchHost(tt.host, tt.pattern, interception.HostMatchTypePrefix); got != tt.want {
			t.Errorf("matchHost(%q, %q, PREFIX) = %v, want %v", tt.host, tt.pattern, got, tt.want)
		}
	}
}

func TestMatchHost_Glob(t *testing.T) {
	tests := []struct {
		host, pattern string
		want          bool
	}{
		{"api.openai.com", "*.openai.com", true}, // * matches non-separator chars
		{"test.com", "*.com", true},
		{"api.openai.com", "api.openai.*", true},
		{"api.openai.com", "api.*.com", true},
		{"www.example.com", "api.*", false},
	}
	for _, tt := range tests {
		if got := matchHost(tt.host, tt.pattern, interception.HostMatchTypeGlob); got != tt.want {
			t.Errorf("matchHost(%q, %q, GLOB) = %v, want %v", tt.host, tt.pattern, got, tt.want)
		}
	}
}

func TestMatchHost_Regex(t *testing.T) {
	tests := []struct {
		host, pattern string
		want          bool
	}{
		{"api.openai.com", `^api\.openai\.com$`, true},
		{"api.openai.com", `\.openai\.com$`, true},
		{"api.anthropic.com", `\.openai\.com$`, false},
	}
	for _, tt := range tests {
		if got := matchHost(tt.host, tt.pattern, interception.HostMatchTypeRegex); got != tt.want {
			t.Errorf("matchHost(%q, %q, REGEX) = %v, want %v", tt.host, tt.pattern, got, tt.want)
		}
	}
}

func TestMatchPath_Exact(t *testing.T) {
	if !matchPath("/v1/chat/completions", "/v1/chat/completions", interception.PathMatchTypeExact) {
		t.Error("expected exact match")
	}
	if matchPath("/v1/chat/completions", "/v1/embeddings", interception.PathMatchTypeExact) {
		t.Error("expected no match")
	}
}

func TestMatchPath_Prefix(t *testing.T) {
	if !matchPath("/v1/chat/completions", "/v1/", interception.PathMatchTypePrefix) {
		t.Error("expected prefix match")
	}
	if matchPath("/v2/chat", "/v1/", interception.PathMatchTypePrefix) {
		t.Error("expected no match")
	}
}

func TestMatchPath_Regex(t *testing.T) {
	if !matchPath("/v1/chat/completions", `^/v1/(chat|embeddings)`, interception.PathMatchTypeRegex) {
		t.Error("expected regex match")
	}
	if matchPath("/v1/files", `^/v1/(chat|embeddings)`, interception.PathMatchTypeRegex) {
		t.Error("expected no match")
	}
}

func TestSortPaths_PriorityThenSpecificity(t *testing.T) {
	paths := []InterceptionPathConfig{
		{ID: "regex-low", MatchType: interception.PathMatchTypeRegex, Priority: 0},
		{ID: "exact-low", MatchType: interception.PathMatchTypeExact, Priority: 0},
		{ID: "prefix-high", MatchType: interception.PathMatchTypePrefix, Priority: 100},
	}
	sortPaths(paths)

	// prefix-high (priority 100) first, then exact-low (specificity 4), then regex-low (specificity 1)
	expected := []string{"prefix-high", "exact-low", "regex-low"}
	for i, p := range paths {
		if p.ID != expected[i] {
			t.Errorf("position %d: got %s, want %s", i, p.ID, expected[i])
		}
	}
}
