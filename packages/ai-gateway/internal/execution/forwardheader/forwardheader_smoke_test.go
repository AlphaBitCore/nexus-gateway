package forwardheader

import "testing"

// TestDefaultsResolveSmoke confirms the embedded defaults.yaml parses
// and resolves cleanly against the canonical adapter type set. This
// is a fast sanity test; the full snapshot + denylist coverage lives
// in forwardheader_test.go (Task 21).
func TestDefaultsResolveSmoke(t *testing.T) {
	cfg := DefaultConfig()
	formats := []string{
		"openai", "deepseek", "glm", "azure-openai", "anthropic",
		"gemini", "minimax", "bedrock", "vertex", "cohere",
		"huggingface", "replicate", "mistral", "xai", "groq",
		"perplexity", "together", "fireworks", "moonshot",
	}
	r, err := Resolve(cfg, formats)
	if err != nil {
		t.Fatalf("Resolve(defaults) returned error: %v", err)
	}

	openai := r.Request("openai")
	for _, want := range []string{"accept", "user-agent", "content-type", "openai-beta", "openai-organization", "openai-project"} {
		if _, ok := openai[want]; !ok {
			t.Errorf("openai request set missing %q", want)
		}
	}

	groq := r.Request("groq")
	if _, ok := groq["openai-beta"]; ok {
		t.Errorf("groq request set must NOT contain openai-beta (per-adapter-type isolation)")
	}
	for _, want := range []string{"accept", "user-agent", "content-type"} {
		if _, ok := groq[want]; !ok {
			t.Errorf("groq request set missing base header %q", want)
		}
	}

	resp := r.Response("openai")
	if _, ok := resp.Static["openai-version"]; !ok {
		t.Errorf("openai response Static missing openai-version")
	}
	if _, ok := resp.PerRequest["x-request-id"]; !ok {
		t.Errorf("openai response PerRequest missing x-request-id")
	}
	if _, ok := resp.Static["x-request-id"]; ok {
		t.Errorf("x-request-id must be PerRequest, not Static")
	}

	if r.Hash() == "" || len(r.Hash()) != 8 {
		t.Errorf("Hash returned %q, want 8 hex chars", r.Hash())
	}
}
