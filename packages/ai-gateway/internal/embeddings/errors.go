package embeddings

import "errors"

// ErrEmbeddingTimeout is returned when the upstream HTTP call to the
// embedding provider exceeds the per-request context deadline.
var ErrEmbeddingTimeout = errors.New("embeddings: request timed out")

// ErrEmbeddingProviderError is returned when the provider responds with
// a non-2xx HTTP status code.
var ErrEmbeddingProviderError = errors.New("embeddings: provider returned an error")

// ErrEmbeddingDimMismatch is returned by Client.Embed when the caller
// passes an expected dimension > 0 and the returned vector length does
// not match.
var ErrEmbeddingDimMismatch = errors.New("embeddings: dimension mismatch between expected and returned vector")
