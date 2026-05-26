// Package embeddings is the dedicated L2-cache embedding client used by the
// semantic cache write+read paths. It calls an OpenAI-compatible /v1/embeddings
// endpoint through a plain http.Client and returns the single (text → vector)
// pair the singleflight key represents.
//
// This package is intentionally narrower than the canonical providers/core
// EmbeddingsRequest: it embeds exactly one text per call (the L2 cache
// fingerprint), runs outside the proxy hot path, and avoids the heavier
// canonical encode/decode round-trips on every cache read. Cross-format batch
// traffic goes through the canonical bridge inside the proxy handler instead.
package embeddings
