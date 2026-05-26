// Package wireformat holds contract tests and validators aligned with vendor
// public HTTP/SSE wire shapes. Official references (consult when updating fixtures):
//
//   - OpenAI Chat Completions (streaming, SSE): https://platform.openai.com/docs/guides/streaming-responses
//   - OpenAI streamed chunk object: https://developers.openai.com/api/reference/resources/chat/subresources/completions/streaming-events/
//   - OpenAI Chat Completions (request body): https://platform.openai.com/docs/api-reference/chat/create
//   - Anthropic Messages streaming (SSE event types): https://docs.anthropic.com/en/api/streaming
//   - Anthropic Messages (request body): https://docs.anthropic.com/en/api/messages
//   - Google Gemini generateContent / streamGenerateContent: https://ai.google.dev/api/generate-content
//   - Gemini streaming (?alt=sse): https://ai.google.dev/gemini-api/docs/text-generation
//
// These tests do not call live vendor endpoints; they validate static JSON
// and SSE frames that match the published wire contracts used by ai-gateway
// adapters and the canonical ingress hub.
//
// Canonical hub field inventory (OpenAI chat.completions 2024-10 JSON as hub)
// lives in package canonicalbridge (CanonicalVersion, SubsetFields).

package wireformat
