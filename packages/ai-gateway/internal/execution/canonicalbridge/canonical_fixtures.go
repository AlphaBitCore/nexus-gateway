package canonicalbridge

import (
	"fmt"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// FixtureProviderModel returns a vendor model id suitable for EncodeRequest
// when exercising the bridge (SelfCheck and matrix tests).
func FixtureProviderModel(f provcore.Format) string {
	switch f {
	case provcore.FormatOpenAI, provcore.FormatDeepSeek, provcore.FormatGLM, provcore.FormatAzureOpenAI:
		return "gpt-4o-mini"
	case provcore.FormatAnthropic:
		return "claude-3-5-haiku-20240307"
	case provcore.FormatGemini, provcore.FormatVertex:
		return "gemini-1.5-flash"
	case provcore.FormatMiniMax:
		return "abab6.5s-chat"
	case provcore.FormatBedrock:
		return "anthropic.claude-3-haiku-20240307-v1:0"
	case provcore.FormatCohere:
		return "command-r-plus"
	case provcore.FormatHuggingFace:
		return "meta-llama/Llama-3.3-70B-Instruct"
	case provcore.FormatReplicate:
		return "meta/llama-3-70b-instruct:abc123"
	case provcore.FormatMistral:
		return "mistral-large-latest"
	case provcore.FormatXai:
		return "grok-2-latest"
	case provcore.FormatGroq:
		return "llama-3.3-70b-versatile"
	case provcore.FormatPerplexity:
		return "sonar-pro"
	case provcore.FormatTogether:
		return "meta-llama/Llama-3.3-70B-Instruct-Turbo"
	case provcore.FormatFireworks:
		return "accounts/fireworks/models/llama-v3p3-70b-instruct"
	case provcore.FormatMoonshot:
		return "moonshot-v1-32k"
	default:
		return "test-model"
	}
}

// MinimalNativeChatBody returns a minimal valid chat request for the given
// ingress wire format, or an error when the format has no hub ingress mapper
// (e.g. Bedrock native ingress is not translated through the hub today).
func MinimalNativeChatBody(ingress provcore.Format) ([]byte, error) {
	switch ingress {
	case provcore.FormatOpenAI, provcore.FormatDeepSeek, provcore.FormatGLM, provcore.FormatAzureOpenAI, provcore.FormatMiniMax:
		return []byte(`{
			"model": "gpt-4o-mini",
			"max_tokens": 32,
			"messages": [{"role": "user", "content": "hello"}]
		}`), nil
	case provcore.FormatAnthropic:
		return []byte(`{
			"model": "claude-3-5-haiku-20240307",
			"max_tokens": 32,
			"messages": [{"role": "user", "content": [{"type": "text", "text": "hello"}]}]
		}`), nil
	case provcore.FormatGemini, provcore.FormatVertex:
		return []byte(`{
			"contents": [{"role": "user", "parts": [{"text": "hello"}]}],
			"generationConfig": {"maxOutputTokens": 32}
		}`), nil
	case provcore.FormatBedrock:
		// Same-format passthrough only; there is no Bedrock ingress→canonical mapper.
		return []byte(`{
			"anthropic_version": "bedrock-2023-05-31",
			"max_tokens": 32,
			"messages": [{"role": "user", "content": [{"type": "text", "text": "hello"}]}]
		}`), nil
	case provcore.FormatCohere:
		return []byte(`{
			"model": "command-r-plus",
			"max_tokens": 32,
			"messages": [{"role": "user", "content": "hello"}]
		}`), nil
	case provcore.FormatHuggingFace:
		return []byte(`{
			"model": "meta-llama/Llama-3.3-70B-Instruct",
			"max_tokens": 32,
			"messages": [{"role": "user", "content": "hello"}]
		}`), nil
	case provcore.FormatReplicate:
		return []byte(`{
			"version": "meta/llama-3-70b-instruct:abc123",
			"input": {
				"prompt": "hello",
				"max_tokens": 32
			}
		}`), nil
	case provcore.FormatMistral, provcore.FormatXai, provcore.FormatGroq,
		provcore.FormatPerplexity, provcore.FormatTogether,
		provcore.FormatFireworks, provcore.FormatMoonshot:
		// All seven OpenAI-compat re-users accept the canonical OpenAI
		// chat shape on the wire; the model field is a hint only and
		// the bridge passes the body through unchanged.
		return []byte(`{
			"model": "test-model",
			"max_tokens": 32,
			"messages": [{"role": "user", "content": "hello"}]
		}`), nil
	default:
		return nil, fmt.Errorf("no fixture for format %q", ingress)
	}
}
