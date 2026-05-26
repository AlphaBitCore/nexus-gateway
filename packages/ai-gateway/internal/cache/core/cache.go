// Package cache implements a Redis-backed response cache for the AI gateway.
// It caches upstream responses (both streaming and non-streaming) keyed by a
// SHA-256 hash of provider, model, and deterministic request fields. Responses
// are stored as schema-discriminated JSON values so a Lookup of one kind
// cannot mis-deserialise the other.
//
// Not to be confused with `cachelayer/` — that package is the in-memory
// snapshot of *config* tables (Provider, Model, Credential, VirtualKey) on
// the hot path. This package caches *response bodies*. Both are "caches" but
// for entirely different domains. See `docs/developers/architecture/cross-cutting/storage/cache-multi-tier-architecture.md`
// for the full multi-tier picture.
package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/redis/go-redis/v9"
)

const (
	defaultTTL    = 1 * time.Hour
	defaultPrefix = "nexus:cache"

	// schemaStream and schemaResponse are stamped into cached values so that
	// LookupStream / LookupResponse can reject a value stored by the other
	// method without relying on caller discipline.
	schemaStream   = "stream/v1"
	schemaResponse = "response/v1"
)

// ErrCacheEntryTooLarge is returned by StoreStream / StoreResponse when the
// serialised entry exceeds the configured maxEntryBytes cap. Callers must
// treat this as "skip cache write" — the live response was already delivered.
var ErrCacheEntryTooLarge = errors.New("cache entry exceeds size cap")

// StreamEntry is the cache value for streaming responses. It preserves the
// original chunk granularity from the producing upstream call so HIT replay
// does not collapse the stream to a single frame.
type StreamEntry struct {
	Schema   string          `json:"schema"` // always schemaStream
	Provider string          `json:"provider"`
	Model    string          `json:"model"` // ProviderModelID
	Chunks   []ChunkRecord   `json:"chunks"`
	Usage    provcore.Usage `json:"usage"`
	CachedAt time.Time       `json:"cachedAt"`
	// UpstreamHeaders preserves upstream HTTP response headers observed at MISS
	// time so HIT replay can run them through the active forward-header
	// allowlist. Stored verbatim — the allowlist is applied at read time so a
	// config change immediately affects what HIT entries surface to the client
	// without requiring invalidation. Per-request headers (request-id,
	// ratelimit-remaining, processing-ms) are always stripped at HIT replay
	// time; storing them is harmless.
	UpstreamHeaders map[string][]string `json:"upstreamHeaders,omitempty"`
	// OriginWireShape encodes both the ingress endpoint kind and body
	// format; tagged so cross-ingress reshape can decide whether to
	// re-encode or serve verbatim. Different ingresses share the same
	// cache key when the canonical body fingerprint matches, so without
	// this tag the HIT path returned the writer's wire shape to every
	// subsequent ingress — the cross-ingress shape contamination bug
	// fixed by Option B2.
	//
	// omitempty so legacy pre-fix entries decode without error: an empty
	// value flags "unknown origin" and the HIT reader falls back to the
	// prior canonical-assuming reshape behavior for those entries.
	OriginWireShape typology.WireShape `json:"originWireShape,omitempty"`
}

// ChunkRecord is the cache-friendly canonical form of a streaming chunk.
//
// RawBytes carries the upstream's exact SSE / NDJSON frame so HIT replay is
// byte-equivalent to the original MISS, preserving the full upstream envelope
// (id, created, model, system_fingerprint, finish_reason, etc.) that strict
// SDK parsers expect.
//
// On the read path chunkSSEReader prefers RawBytes when present
// (same-ingress fast path). The canonical Delta / ReasoningDelta /
// ToolCallDeltas / Usage fields stay populated alongside RawBytes so future
// cross-ingress replay and hook content extraction can read them without
// parsing RawBytes.
//
// omitempty everywhere keeps stream entries compact when individual chunks
// carry only deltas (the common case).
type ChunkRecord struct {
	Delta          string                    `json:"d,omitempty"`
	ReasoningDelta string                    `json:"r,omitempty"`
	ToolCallDeltas []provcore.ToolCallDelta `json:"t,omitempty"`
	Usage          *provcore.Usage          `json:"u,omitempty"`
	Done           bool                      `json:"done,omitempty"`
	NativeEvent    string                    `json:"e,omitempty"`
	RawBytes       []byte                    `json:"raw,omitempty"`
}

// ResponseEntry is the cache value for non-streaming responses.
type ResponseEntry struct {
	Schema            string          `json:"schema"` // always schemaResponse
	Provider          string          `json:"provider"`
	Model             string          `json:"model"`
	CanonicalResponse json.RawMessage `json:"response"`
	Usage             provcore.Usage `json:"usage"`
	CachedAt          time.Time       `json:"cachedAt"`
	// UpstreamHeaders — same semantics as on StreamEntry.
	UpstreamHeaders map[string][]string `json:"upstreamHeaders,omitempty"`
	// OriginWireShape encodes both the ingress endpoint kind and body
	// format; tagged so cross-ingress reshape can decide whether to
	// re-encode or serve verbatim. See StreamEntry for the full
	// rationale.
	OriginWireShape typology.WireShape `json:"originWireShape,omitempty"`
}

// Config controls cache behaviour at construction time. After construction the
// enabled flag, TTL, and applyFreshnessRules can be swapped at runtime via
// SetConfig (driven by the response_cache.extract_config Hub shadow key).
// Prefix and MaxEntryBytes are yaml-only because changing them at runtime
// would invalidate existing entries.
type Config struct {
	Enabled       bool          `yaml:"enabled"`
	TTL           time.Duration `yaml:"ttl"`
	Prefix        string        `yaml:"prefix"`
	MaxEntryBytes int           `yaml:"maxEntryBytes"` // 0 → 1 MiB default
	// ApplyFreshnessRules gates whether classifyCachePreLookup honours a
	// freshness-detector match by returning (Skipped, time_sensitive) for
	// BOTH L1 and L2. The proxy reads this via ApplyFreshnessRules() at
	// classify time. Hot-swapped by SetConfig.
	ApplyFreshnessRules bool `yaml:"applyFreshnessRules"`
}

// ConfigSnapshot is the runtime-swappable subset of Config. configdispatch
// builds one of these from the response_cache.extract_config Hub blob and
// hands it to SetConfig.
type ConfigSnapshot struct {
	Enabled             bool
	TTL                 time.Duration
	ApplyFreshnessRules bool
}

// Cache is a Redis-backed response cache. A nil *Cache is safe to use — all
// methods are no-ops on a nil receiver.
//
// enabled / ttl / applyFreshnessRules are stored as atomics so the
// configdispatch handler can hot-swap them without restarting the service.
type Cache struct {
	rdb           redis.UniversalClient
	prefix        string
	logger        *slog.Logger
	maxEntryBytes int

	enabled             atomic.Bool
	ttlNs               atomic.Int64 // nanoseconds
	applyFreshnessRules atomic.Bool
}

// New creates a Cache backed by the given Redis client. Returns nil (cache
// not wired) only when rdb is nil — without a Redis client there's nothing
// to back the cache and all callers should treat that as "no cache feature".
//
// cfg.Enabled is honoured as the *initial* enabled flag; the
// configdispatch handler will overwrite it on the first Hub push.
// cfg.ApplyFreshnessRules likewise. cfg.Prefix and cfg.MaxEntryBytes are
// permanent (set once at boot from yaml).
func New(rdb redis.UniversalClient, cfg Config, logger *slog.Logger) *Cache {
	if rdb == nil {
		return nil
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = defaultTTL
	}
	prefix := cfg.Prefix
	if prefix == "" {
		prefix = defaultPrefix
	}
	maxBytes := cfg.MaxEntryBytes
	if maxBytes <= 0 {
		maxBytes = 1 << 20 // 1 MiB
	}
	c := &Cache{
		rdb:           rdb,
		prefix:        prefix,
		logger:        logger,
		maxEntryBytes: maxBytes,
	}
	c.enabled.Store(cfg.Enabled)
	c.ttlNs.Store(int64(ttl))
	c.applyFreshnessRules.Store(cfg.ApplyFreshnessRules)
	return c
}

// SetConfig atomically updates the hot-swappable runtime config.
// Safe to call concurrently with hot-path Lookup/Store calls. Pass a zero TTL
// to keep the existing TTL unchanged.
func (c *Cache) SetConfig(snap ConfigSnapshot) {
	if c == nil {
		return
	}
	c.enabled.Store(snap.Enabled)
	if snap.TTL > 0 {
		c.ttlNs.Store(int64(snap.TTL))
	}
	c.applyFreshnessRules.Store(snap.ApplyFreshnessRules)
}

// IsEnabled returns the current runtime enabled state. Safe on a nil
// receiver (returns false).
func (c *Cache) IsEnabled() bool {
	if c == nil {
		return false
	}
	return c.enabled.Load()
}

// ApplyFreshnessRules returns whether the proxy's classifyCachePreLookup
// should honour freshness-detector matches by skipping cache. Safe on nil.
func (c *Cache) ApplyFreshnessRules() bool {
	if c == nil {
		return false
	}
	return c.applyFreshnessRules.Load()
}

// ttl returns the current runtime TTL as a time.Duration.
func (c *Cache) ttl() time.Duration {
	if c == nil {
		return 0
	}
	return time.Duration(c.ttlNs.Load())
}

// canonicalizeJSON returns body with JSON object keys sorted recursively.
// Array element order, scalar values, and field names are preserved
// exactly. The "stream" / "stream_options" fields are intentionally
// retained — design D4 keeps streaming and non-streaming cache entries
// on disjoint hashes because upstream stream / non-stream endpoints are
// not always semantically equivalent.
//
// If body is not valid JSON, it is returned unchanged so non-JSON
// bodies (e.g. Bedrock binary) hash deterministically.
func canonicalizeJSON(body []byte) []byte {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	out, err := marshalSortedKeys(v)
	if err != nil {
		return body
	}
	return out
}

// marshalSortedKeys recursively marshals a JSON value, sorting object
// keys alphabetically at every nesting level. Array element order is
// preserved — message ordering is semantically load-bearing in chat
// conversations.
func marshalSortedKeys(v any) ([]byte, error) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var buf bytes.Buffer
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			vb, err := marshalSortedKeys(x[k])
			if err != nil {
				return nil, err
			}
			buf.Write(vb)
		}
		buf.WriteByte('}')
		return buf.Bytes(), nil
	case []any:
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			ib, err := marshalSortedKeys(item)
			if err != nil {
				return nil, err
			}
			buf.Write(ib)
		}
		buf.WriteByte(']')
		return buf.Bytes(), nil
	default:
		return json.Marshal(x)
	}
}

// BuildKey produces a deterministic cache key from
// (provider, model, body, allowlistVersion). The body MUST be the bytes sent
// to upstream (output of Adapter.PrepareBody, not the raw client body) so that
// equivalent requests with different client model aliases, ingress shapes, or
// SDK JSON orderings hash to the same key.
//
// allowlistVersion is the forwardheader.Resolved.Hash of the active
// forward-header allowlist. Folding it into the key guarantees that a YAML
// config change invalidates entries whose UpstreamHeaders were observed under
// a different effective filter. Pass an empty string when the allowlist is
// not relevant (tests, non-forwarding contexts).
//
// The "v3\n" header pins the key schema; v1/v2 entries are unreachable.
func (c *Cache) BuildKey(provider, model string, body []byte, allowlistVersion string) string {
	if c == nil {
		return ""
	}
	canonical := canonicalizeJSON(body)
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "v3\nprovider=%s\nmodel=%s\nallowlist=%s\nbody=", provider, model, allowlistVersion)
	h.Write(canonical)
	return c.prefix + ":" + hex.EncodeToString(h.Sum(nil))
}

// StoreStream persists a StreamEntry with the schema discriminator stamped.
// Returns (n, ErrCacheEntryTooLarge) if the serialised size exceeds
// maxEntryBytes; callers treat that as "skip cache write" — the live
// response was already delivered. On success n is the number of serialised
// bytes written to Redis.
func (c *Cache) StoreStream(ctx context.Context, key string, entry *StreamEntry) (int, error) {
	if c == nil || key == "" || entry == nil || !c.enabled.Load() {
		return 0, nil
	}
	entry.Schema = schemaStream
	data, err := json.Marshal(entry)
	if err != nil {
		c.logger.Warn("cache stream marshal error", "key", key, "error", err)
		return 0, err
	}
	if c.maxEntryBytes > 0 && len(data) > c.maxEntryBytes {
		return len(data), ErrCacheEntryTooLarge
	}
	if err := c.rdb.Set(ctx, key, data, c.ttl()).Err(); err != nil {
		c.logger.Warn("cache stream store error", "key", key, "error", err)
		return 0, err
	}
	return len(data), nil
}

// StoreResponse persists a ResponseEntry with the schema discriminator stamped.
// Returns (n, ErrCacheEntryTooLarge) on oversize. On success n is the number
// of serialised bytes written to Redis.
func (c *Cache) StoreResponse(ctx context.Context, key string, entry *ResponseEntry) (int, error) {
	if c == nil || key == "" || entry == nil || !c.enabled.Load() {
		return 0, nil
	}
	entry.Schema = schemaResponse
	data, err := json.Marshal(entry)
	if err != nil {
		c.logger.Warn("cache response marshal error", "key", key, "error", err)
		return 0, err
	}
	if c.maxEntryBytes > 0 && len(data) > c.maxEntryBytes {
		return len(data), ErrCacheEntryTooLarge
	}
	if err := c.rdb.Set(ctx, key, data, c.ttl()).Err(); err != nil {
		c.logger.Warn("cache response store error", "key", key, "error", err)
		return 0, err
	}
	return len(data), nil
}

// LookupStream returns the StreamEntry for key, or nil on miss, schema
// mismatch, or decode error.
func (c *Cache) LookupStream(ctx context.Context, key string) *StreamEntry {
	if c == nil || key == "" || !c.enabled.Load() {
		return nil
	}
	data, err := c.rdb.Get(ctx, key).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			c.logger.Warn("cache lookup error", "key", key, "error", err)
		}
		return nil
	}
	var probe struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil
	}
	if probe.Schema != schemaStream {
		return nil
	}
	var entry StreamEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil
	}
	return &entry
}

// LookupResponse returns the ResponseEntry for key, or nil on miss (including
// runtime-disabled state from SetConfig), schema
// mismatch, or decode error.
func (c *Cache) LookupResponse(ctx context.Context, key string) *ResponseEntry {
	if c == nil || key == "" || !c.enabled.Load() {
		return nil
	}
	data, err := c.rdb.Get(ctx, key).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			c.logger.Warn("cache lookup error", "key", key, "error", err)
		}
		return nil
	}
	var probe struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil
	}
	if probe.Schema != schemaResponse {
		return nil
	}
	var entry ResponseEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil
	}
	return &entry
}

// Flush removes all cached entries matching the given provider. If provider is
// empty, all entries under the configured prefix are removed. Returns the
// number of keys deleted.
func (c *Cache) Flush(ctx context.Context, provider string) (int64, error) {
	if c == nil {
		return 0, nil
	}

	pattern := c.prefix + ":*"
	// Provider-scoped flush is not possible with our key scheme (keys are
	// SHA-256 hashes), so we always flush the entire prefix. The provider
	// parameter is accepted for future use with tagged keys.
	_ = provider

	var deleted int64
	var cursor uint64
	for {
		keys, nextCursor, err := c.rdb.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return deleted, fmt.Errorf("cache flush scan: %w", err)
		}
		if len(keys) > 0 {
			n, err := c.rdb.Del(ctx, keys...).Result()
			if err != nil {
				return deleted, fmt.Errorf("cache flush del: %w", err)
			}
			deleted += n
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return deleted, nil
}

// Stats returns approximate hit count, miss count, and total cached entry
// count. Hit/miss counters are maintained via Redis INCR on sidecar keys.
func (c *Cache) Stats(ctx context.Context) (hits, misses, size int64) {
	if c == nil {
		return 0, 0, 0
	}
	hits, _ = c.rdb.Get(ctx, c.prefix+":stats:hits").Int64()
	misses, _ = c.rdb.Get(ctx, c.prefix+":stats:misses").Int64()

	// Count keys matching the cache prefix (excluding stats keys).
	var count int64
	var cursor uint64
	for {
		keys, nextCursor, err := c.rdb.Scan(ctx, cursor, c.prefix+":*", 100).Result()
		if err != nil {
			break
		}
		for _, k := range keys {
			if k != c.prefix+":stats:hits" && k != c.prefix+":stats:misses" {
				count++
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	size = count
	return hits, misses, size
}

// RecordHit increments the cache hit counter.
func (c *Cache) RecordHit(ctx context.Context) {
	if c == nil {
		return
	}
	c.rdb.Incr(ctx, c.prefix+":stats:hits")
}

// RecordMiss increments the cache miss counter.
func (c *Cache) RecordMiss(ctx context.Context) {
	if c == nil {
		return
	}
	c.rdb.Incr(ctx, c.prefix+":stats:misses")
}
