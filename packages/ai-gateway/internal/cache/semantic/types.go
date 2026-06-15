package semantic

import (
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// StoreInput carries all the data needed to write a single L2 semantic
// cache entry via Client.StoreEntry. The caller populates this from the
// L1 write path after a successful upstream response.
type StoreInput struct {
	// Identity
	VKScope          string // "v1:vk:<id>" or "v1:user:<id>" etc.
	UpstreamProvider string // e.g., "openai"
	UpstreamModel    string // e.g., "gpt-4o-mini"
	ResponseKind     string // "response" | "stream"
	Fingerprint      string // ConfigCache fingerprint at time of write

	// Vector
	EmbeddingInput string    // exact text fed to the embedding model
	Embedding      []float32 // float32 vector produced by the embedding model

	// AllowCrossModel mirrors the fleet semantic policy at write time. It
	// controls whether upstream_provider + upstream_model are folded into the
	// entry key: when false (strict, the default) each (provider, model) gets
	// its own key so entries for distinct models do not mutually evict; when
	// true the model is interchangeable for retrieval, so it is left out of the
	// key. See entryKey.
	AllowCrossModel bool

	// Payload
	ResponseBody []byte         // canonical bytes (response) or JSON ChunkRecord array (stream)
	Usage        map[string]any // canonical usage stamps
	TTL          time.Duration

	// OriginWireShape encodes both the ingress endpoint kind and body
	// format; tagged so cross-ingress reshape can decide whether to
	// re-encode or serve verbatim. Persisted alongside the entry so a
	// HIT served to a different ingress can reshape via canonicalbridge
	// instead of leaking the writer's wire shape. See cache/core.ResponseEntry.
	OriginWireShape typology.WireShape
}

// LookupInput carries the parameters for a KNN vector search.
// Defined here so the Writer and Client share one type definition.
type LookupInput struct {
	VKScope          string
	UpstreamProvider string
	UpstreamModel    string // may be ignored when AllowCrossModel is true
	ResponseKind     string
	Fingerprint      string // ConfigCache fingerprint at lookup time

	Embedding       []float32
	Threshold       float32 // minimum cosine similarity to qualify as a hit (e.g. 0.96)
	AllowCrossModel bool    // when true, upstream_model filter is dropped
}

// Entry is returned by a successful L2 semantic cache lookup.
type Entry struct {
	ResponseBody     []byte
	Usage            map[string]any
	Similarity       float32
	CachedAt         time.Time
	UpstreamProvider string
	UpstreamModel    string
	Fingerprint      string
	// EntryKey is the Redis HASH key for this entry (e.g. "<index>:<hash>").
	// Returned so callers can pass it to PoisonList.Add on negative feedback.
	EntryKey string

	// OriginWireShape encodes both the ingress endpoint kind and body
	// format; tagged so cross-ingress reshape can decide whether to
	// re-encode or serve verbatim. Replays the ingress wire shape
	// stamped at write time. Empty for pre-fix entries: the reader falls
	// back to the prior canonical-assuming reshape behavior.
	OriginWireShape typology.WireShape
}
