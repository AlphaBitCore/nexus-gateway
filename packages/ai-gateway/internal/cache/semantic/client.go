package semantic

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// defaultMaxEntryBytes is the default maximum serialised entry size
	// (response_body + usage + other fields). Per response-cache-architecture.md
	// §3.9 item 3a: 256 KiB is stricter than L1's 1 MiB because L2 stores
	// per-entry vector + tag fields with overhead.
	defaultMaxEntryBytes = 256 * 1024

	// keyHashLen is the number of hex characters taken from the SHA-256 of the
	// EmbeddingInput to form the entry key suffix. 16 chars = 64 bits of
	// collision resistance — vanishingly unlikely for the entry counts we
	// target (< 10M).
	keyHashLen = 16

	// indexAlreadyExistsMsg is the error prefix returned by valkey-search
	// when FT.CREATE is called on an existing index.
	indexAlreadyExistsMsg = "Index already exists"

	// indexNotFoundMsg is the error prefix from valkey-search on FT.DROPINDEX
	// for a non-existent index.
	indexNotFoundMsg = "Unknown index name"
)

// Client wraps the Valkey client and exposes index management + entry
// storage for the L2 semantic cache. Thread-safe; all exported methods
// may be called concurrently.
type Client struct {
	rdb *redis.Client
	log *slog.Logger
	ns  string // Prometheus namespace (unused in this file; kept for future metrics)
	cb  *CircuitBreaker
}

// NewClient constructs a Client.
//   - rdb: the shared *redis.Client pointing at the Valkey instance.
//   - log: service-level slog.Logger; never nil.
//   - namespace: Prometheus namespace (e.g. "nexus").
//   - cb: circuit breaker for write-path protection (may be nil — no protection).
func NewClient(rdb *redis.Client, log *slog.Logger, namespace string, cb *CircuitBreaker) *Client {
	return &Client{
		rdb: rdb,
		log: log,
		ns:  namespace,
		cb:  cb,
	}
}

// EnsureIndex runs FT.CREATE if the named index does not exist. It is
// idempotent: if the index already exists, EnsureIndex logs a debug
// message and returns nil.
//
// The HNSW schema matches response-cache-architecture.md §3.5:
//
//	SCHEMA
//	  vector            VECTOR HNSW 12 DIM <dim> TYPE FLOAT32 DISTANCE_METRIC COSINE M 16 EF_CONSTRUCTION 200 EF_RUNTIME 10
//	  upstream_provider TAG
//	  upstream_model    TAG
//	  vk_scope          TAG
//	  response_kind     TAG
//	  fingerprint       TAG
//	  response_body     TEXT NOINDEX
//	  usage             TEXT NOINDEX
//	  cached_at         NUMERIC
func (c *Client) EnsureIndex(ctx context.Context, indexName string, dim int) error {
	if indexName == "" {
		return fmt.Errorf("semantic/client: EnsureIndex: indexName is empty")
	}
	if dim <= 0 {
		return fmt.Errorf("semantic/client: EnsureIndex: dim must be > 0, got %d", dim)
	}

	// FT.CREATE <indexName> ON HASH PREFIX 1 "<indexName>:"
	//   SCHEMA
	//     vector            VECTOR HNSW 12 DIM <dim> TYPE FLOAT32 DISTANCE_METRIC COSINE M 16 EF_CONSTRUCTION 200 EF_RUNTIME 10
	//     upstream_provider TAG
	//     upstream_model    TAG
	//     vk_scope          TAG
	//     response_kind     TAG
	//     fingerprint       TAG
	//     cached_at         NUMERIC
	//
	// response_body / usage intentionally OMITTED from the SCHEMA: they are
	// payload-only hash fields (Reader pulls them via FT.SEARCH ... RETURN,
	// which works against unindexed hash fields). Including them as
	// `TEXT NOINDEX` is rejected by Valkey 8.x's open-source search module
	// ("Invalid field type … Unknown argument `TEXT`") — RedisSearch-style
	// TEXT type is not part of the Valkey search module. Storage works
	// regardless; indexing them would only be needed for full-text search
	// we don't perform.
	args := []interface{}{
		"FT.CREATE", indexName,
		"ON", "HASH",
		"PREFIX", "1", indexName + ":",
		"SCHEMA",
		"vector", "VECTOR", "HNSW", "12",
		"DIM", fmt.Sprintf("%d", dim),
		"TYPE", "FLOAT32",
		"DISTANCE_METRIC", "COSINE",
		"M", "16",
		"EF_CONSTRUCTION", "200",
		"EF_RUNTIME", "10",
		"upstream_provider", "TAG",
		"upstream_model", "TAG",
		"vk_scope", "TAG",
		"response_kind", "TAG",
		"fingerprint", "TAG",
		"cached_at", "NUMERIC",
	}

	err := c.rdb.Do(ctx, args...).Err()
	if err != nil {
		if isIndexExistsError(err) {
			c.log.Debug("semantic/client: EnsureIndex: index already exists, skipping",
				"index", indexName, "dim", dim)
			return nil
		}
		return fmt.Errorf("%w: FT.CREATE %q: %w", ErrValkeyUnavailable, indexName, err)
	}
	c.log.Info("semantic/client: EnsureIndex: created index",
		"index", indexName, "dim", dim)
	return nil
}

// DropIndex runs FT.DROPINDEX for the named index. It is idempotent: if
// the index does not exist, DropIndex logs a debug message and returns nil.
func (c *Client) DropIndex(ctx context.Context, indexName string) error {
	if indexName == "" {
		return fmt.Errorf("semantic/client: DropIndex: indexName is empty")
	}

	err := c.rdb.Do(ctx, "FT.DROPINDEX", indexName).Err()
	if err != nil {
		if isIndexMissingError(err) {
			c.log.Debug("semantic/client: DropIndex: index not found, skipping",
				"index", indexName)
			return nil
		}
		return fmt.Errorf("%w: FT.DROPINDEX %q: %w", ErrValkeyUnavailable, indexName, err)
	}
	c.log.Info("semantic/client: DropIndex: dropped index", "index", indexName)
	return nil
}

// StoreEntry writes a single L2 semantic cache entry to Valkey via HSET.
//
// Key format: <indexName>:<sha256(EmbeddingInput | VKScope | ResponseKind
// [| UpstreamProvider | UpstreamModel])[:16]>. The scope/kind (and, when
// AllowCrossModel is false, provider+model) are folded into the key so that
// the same embedding text written under different tenants, models, or
// response kinds occupies distinct HASH keys instead of mutually evicting
// via HSET overwrite. Reads do not reconstruct the key — FT.SEARCH locates
// the entry by vector + tag filter and returns the stored key — so the key
// composition is purely a write-side uniqueness concern.
//
// The Embedding is encoded as FLOAT32 little-endian bytes (matching
// valkey-search's binary vector blob expectation).
//
// Returns ErrEntryTooLarge when the serialised payload exceeds maxEntryBytes.
// Returns ErrValkeyUnavailable on connection / command errors.
func (c *Client) StoreEntry(ctx context.Context, indexName string, in StoreInput, maxEntryBytes int) error {
	if maxEntryBytes <= 0 {
		maxEntryBytes = defaultMaxEntryBytes
	}

	// Size cap check — response_body is the dominant contributor.
	if len(in.ResponseBody) > maxEntryBytes {
		return fmt.Errorf("%w: response_body %d > %d",
			ErrEntryTooLarge, len(in.ResponseBody), maxEntryBytes)
	}

	// Encode vector as FLOAT32 little-endian bytes.
	vecBytes := float32sToBytes(in.Embedding)

	// Encode usage as JSON.
	usageJSON := []byte("{}")
	if len(in.Usage) > 0 {
		var err error
		usageJSON, err = json.Marshal(in.Usage)
		if err != nil {
			return fmt.Errorf("semantic/client: StoreEntry: marshal usage: %w", err)
		}
	}

	// Build the hash key.
	entryKey := entryKey(indexName, in)

	// HSET with all fields.
	fields := map[string]interface{}{
		"vector":            string(vecBytes),
		"upstream_provider": in.UpstreamProvider,
		"upstream_model":    in.UpstreamModel,
		"vk_scope":          in.VKScope,
		"response_kind":     in.ResponseKind,
		"fingerprint":       in.Fingerprint,
		"response_body":     string(in.ResponseBody),
		"usage":             string(usageJSON),
		"cached_at":         fmt.Sprintf("%d", time.Now().Unix()),
		"origin_wire_shape": string(in.OriginWireShape),
	}

	if err := c.rdb.HSet(ctx, entryKey, fields).Err(); err != nil {
		return fmt.Errorf("%w: HSET %q: %w", ErrValkeyUnavailable, entryKey, err)
	}

	// Set TTL via PEXPIRE (millisecond precision).
	if in.TTL > 0 {
		if err := c.rdb.PExpire(ctx, entryKey, in.TTL).Err(); err != nil {
			// Non-fatal: the entry was written; expiry is best-effort.
			c.log.Warn("semantic/client: StoreEntry: PEXPIRE failed",
				"key", entryKey, "ttl", in.TTL, "error", err)
		}
	}

	c.log.Debug("semantic/client: StoreEntry: wrote entry",
		"key", entryKey,
		"provider", in.UpstreamProvider,
		"model", in.UpstreamModel,
		"kind", in.ResponseKind,
		"ttl", in.TTL,
		"bodyBytes", len(in.ResponseBody),
	)
	return nil
}

// Key helpers

// entryKey returns the Redis HASH key for an L2 entry. The hash folds the
// embedding input together with the entry's scope (vk_scope), response kind,
// and — unless AllowCrossModel is set — its upstream provider + model, so
// that logically-distinct entries that happen to share embedding text do not
// collide on a single key and evict each other via HSET overwrite.
//
// A NUL separator delimits the components so concatenation is unambiguous
// (no value can contain a NUL byte). When AllowCrossModel is true the model
// is interchangeable for retrieval, so provider+model are omitted and the
// newest response for a given (input, scope, kind) supersedes the prior one.
func entryKey(indexName string, in StoreInput) string {
	var sb strings.Builder
	sb.WriteString(in.EmbeddingInput)
	sb.WriteByte(0)
	sb.WriteString(in.VKScope)
	sb.WriteByte(0)
	sb.WriteString(in.ResponseKind)
	if !in.AllowCrossModel {
		sb.WriteByte(0)
		sb.WriteString(in.UpstreamProvider)
		sb.WriteByte(0)
		sb.WriteString(in.UpstreamModel)
	}
	h := sha256.Sum256([]byte(sb.String()))
	hex := fmt.Sprintf("%x", h)
	return indexName + ":" + hex[:keyHashLen]
}

// Vector encoding helpers

// float32sToBytes encodes []float32 as FLOAT32 little-endian bytes.
// This matches the binary blob format expected by valkey-search.
func float32sToBytes(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// Error classifiers

func isIndexExistsError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, indexAlreadyExistsMsg)
}

func isIndexMissingError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, indexNotFoundMsg) ||
		errors.Is(err, ErrIndexMissing)
}
