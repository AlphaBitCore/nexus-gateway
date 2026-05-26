package aiguard

import (
	"strings"
	"testing"
)

func TestBackendFingerprint_Deterministic(t *testing.T) {
	a := BackendFingerprint("configured_provider", "openai", "gpt-4o-mini", "prompt-v1")
	b := BackendFingerprint("configured_provider", "openai", "gpt-4o-mini", "prompt-v1")
	if a != b {
		t.Fatalf("fingerprint not deterministic: %q vs %q", a, b)
	}
}

func TestBackendFingerprint_ChangesWithEachInput(t *testing.T) {
	base := BackendFingerprint("configured_provider", "openai", "gpt-4o-mini", "prompt-v1")
	variants := map[string]string{
		"mode":     BackendFingerprint("external_url", "openai", "gpt-4o-mini", "prompt-v1"),
		"provider": BackendFingerprint("configured_provider", "anthropic", "gpt-4o-mini", "prompt-v1"),
		"model":    BackendFingerprint("configured_provider", "openai", "gpt-4o", "prompt-v1"),
		"prompt":   BackendFingerprint("configured_provider", "openai", "gpt-4o-mini", "prompt-v2"),
	}
	for label, v := range variants {
		if v == base {
			t.Errorf("fingerprint unchanged when %s changed: %q", label, v)
		}
	}
}

func TestBackendFingerprint_Format(t *testing.T) {
	fp := BackendFingerprint("configured_provider", "openai", "gpt-4o-mini", "prompt-v1")
	if len(fp) != 64 {
		t.Fatalf("want 64-char hex, got %d: %q", len(fp), fp)
	}
	if strings.ContainsAny(fp, "ABCDEF") {
		t.Errorf("expected lowercase hex, got %q", fp)
	}
}

func TestPromptTemplateSHA_Format(t *testing.T) {
	s := PromptTemplateSHA("hello")
	if len(s) != 64 {
		t.Fatalf("want 64-char hex, got %d", len(s))
	}
}
