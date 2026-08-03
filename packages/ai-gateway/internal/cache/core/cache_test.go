package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestCache returns a *Cache that is non-nil for BuildKey purposes
// but has no Redis client wired. Lookup/Store would NPE; only call
// BuildKey on this instance.
func newTestCache(prefix string) *Cache {
	c := &Cache{
		rdb:    nil,
		prefix: prefix,
		logger: slog.Default(),
	}
	c.enabled.Store(true)
	c.ttlNs.Store(int64(defaultTTL))
	return c
}

// newTestCacheWithRedis returns a *Cache backed by a miniredis instance.
// The instance is automatically stopped via t.Cleanup.
func newTestCacheWithRedis(t *testing.T) *Cache {
	t.Helper()
	s, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	c := New(rdb, Config{
		Enabled: true,
		TTL:     time.Minute,
		Prefix:  "test:" + t.Name(),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return c
}

// BuildKey tests (pre-existing, kept verbatim)

func TestBuildKey_Deterministic(t *testing.T) {
	c := newTestCache("ai-gw:")
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"temperature":0.7}`)

	k1 := c.BuildKey("openai", "gpt-4", body, "")
	k2 := c.BuildKey("openai", "gpt-4", body, "")
	if k1 != k2 {
		t.Fatalf("BuildKey not deterministic: %q != %q", k1, k2)
	}
	if !strings.HasPrefix(k1, "ai-gw:") {
		t.Fatalf("BuildKey prefix missing: %q", k1)
	}
}

// TestBuildKey_DistinguishesProviderModel verifies that swapping just
// the provider or just the model produces a different key, even when
// the body is byte-identical.
func TestBuildKey_DistinguishesProviderModel(t *testing.T) {
	c := newTestCache("ai-gw:")
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)

	base := c.BuildKey("openai", "gpt-4", body, "")

	cases := []struct {
		name            string
		provider, model string
	}{
		{"different provider", "anthropic", "gpt-4"},
		{"different model", "openai", "gpt-4o"},
		{"both different", "anthropic", "claude-3-5-sonnet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.BuildKey(tc.provider, tc.model, body, "")
			if got == base {
				t.Fatalf("BuildKey collided: %q == %q", got, base)
			}
		})
	}
}

// TestBuildKey_FullBodySensitive is the regression guard for the
// previous selective-field hash. Two bodies with identical messages /
// temperature / max_tokens but differing in tools / system / seed /
// top_p / response_format MUST hash to different keys, otherwise we
// risk serving a cached response that does not match the upstream
// behavior the new fields would produce.
func TestBuildKey_FullBodySensitive(t *testing.T) {
	c := newTestCache("ai-gw:")
	base := []byte(`{"messages":[{"role":"user","content":"hi"}],"temperature":0.7,"max_tokens":100}`)
	baseKey := c.BuildKey("openai", "gpt-4", base, "")

	// Each variant adds or changes a field NOT covered by the old
	// (messages, temperature, max_tokens) extraction. Pre-fix, all
	// of these would have collided with `base`.
	variants := map[string][]byte{
		"adds tools":           []byte(`{"messages":[{"role":"user","content":"hi"}],"temperature":0.7,"max_tokens":100,"tools":[{"type":"function","function":{"name":"f"}}]}`),
		"adds system field":    []byte(`{"messages":[{"role":"user","content":"hi"}],"temperature":0.7,"max_tokens":100,"system":"you are concise"}`),
		"adds seed":            []byte(`{"messages":[{"role":"user","content":"hi"}],"temperature":0.7,"max_tokens":100,"seed":42}`),
		"adds top_p":           []byte(`{"messages":[{"role":"user","content":"hi"}],"temperature":0.7,"max_tokens":100,"top_p":0.9}`),
		"adds response_format": []byte(`{"messages":[{"role":"user","content":"hi"}],"temperature":0.7,"max_tokens":100,"response_format":{"type":"json_object"}}`),
		"adds tool_choice":     []byte(`{"messages":[{"role":"user","content":"hi"}],"temperature":0.7,"max_tokens":100,"tool_choice":"auto"}`),
		"different message":    []byte(`{"messages":[{"role":"user","content":"hello"}],"temperature":0.7,"max_tokens":100}`),
		"different temp":       []byte(`{"messages":[{"role":"user","content":"hi"}],"temperature":0.8,"max_tokens":100}`),
	}
	for name, b := range variants {
		t.Run(name, func(t *testing.T) {
			got := c.BuildKey("openai", "gpt-4", b, "")
			if got == baseKey {
				t.Fatalf("body variant %q collided with base key", name)
			}
		})
	}
}

// TestBuildKey_ByteIdenticalProducesSameKey is a positive control —
// re-encoding the same body MUST yield the same key. We do NOT
// canonicalize JSON; whitespace differences will produce different
// keys (acceptable, since hooks are the canonicalizer).
func TestBuildKey_ByteIdenticalProducesSameKey(t *testing.T) {
	c := newTestCache("ai-gw:")
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	if c.BuildKey("openai", "gpt-4", body, "") != c.BuildKey("openai", "gpt-4", append([]byte{}, body...), "") {
		t.Fatal("byte-identical bodies produced different keys")
	}
}

func TestBuildKey_NilReceiverEmptyKey(t *testing.T) {
	var c *Cache
	if got := c.BuildKey("openai", "gpt-4", []byte(`{}`), ""); got != "" {
		t.Fatalf("nil cache BuildKey = %q, want empty", got)
	}
}

func TestBuildKey_V3_PrefixIsV3(t *testing.T) {
	// Deterministic recipe check via direct hashing. Schema bumped to
	// v3 to fold the forward-header allowlist version into the cache key
	// (so a YAML config change invalidates entries).
	c := newTestCache("ai-gw:")
	body := []byte(`{"messages":[]}`)
	got := c.BuildKey("openai", "gpt-4o", body, "abcd1234")

	// Expected manually-computed key with v3 prefix.
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "v3\nprovider=%s\nmodel=%s\nallowlist=%s\nbody=", "openai", "gpt-4o", "abcd1234")
	h.Write([]byte(`{"messages":[]}`)) // already canonical
	want := "ai-gw:" + ":" + hex.EncodeToString(h.Sum(nil))

	if got != want {
		t.Fatalf("v3 prefix mismatch:\n got=%s\nwant=%s", got, want)
	}
}

// TestBuildKey_V3_AllowlistVersionAffectsKey confirms that changing
// the forward-header allowlist version re-keys the cache. Two requests
// identical in every other dimension must hash to different cache keys
// when allowlistVersion differs.
func TestBuildKey_V3_AllowlistVersionAffectsKey(t *testing.T) {
	c := newTestCache("ai-gw:")
	body := []byte(`{"messages":[]}`)
	k1 := c.BuildKey("openai", "gpt-4o", body, "v1hash")
	k2 := c.BuildKey("openai", "gpt-4o", body, "v2hash")
	if k1 == k2 {
		t.Fatalf("BuildKey did not fold allowlistVersion: %s", k1)
	}
}

// TestBuildScopedKey_TenantIsolation is the regression guard.
// Two requests with byte-identical bodies but different tenant scopes must hash
// to DIFFERENT L1 cache keys when a scope is folded in, while an empty scope
// preserves fleet-wide dedup AND is byte-identical to the legacy BuildKey.
func TestBuildScopedKey_TenantIsolation(t *testing.T) {
	c := newTestCache("ai-gw:")
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"temperature":0.7}`)

	// Two different VK scopes, identical body → different keys (isolation).
	kVKA := c.BuildScopedKey("openai", "gpt-4o", body, "", "vk:vk-A")
	kVKB := c.BuildScopedKey("openai", "gpt-4o", body, "", "vk:vk-B")
	if kVKA == kVKB {
		t.Fatalf("scoped keys collided across VKs: %q == %q", kVKA, kVKB)
	}

	// Same VK scope, identical body → same key (dedup within tenant).
	if got := c.BuildScopedKey("openai", "gpt-4o", body, "", "vk:vk-A"); got != kVKA {
		t.Fatalf("same-scope keys differ: %q != %q", got, kVKA)
	}

	// Empty scope (vary_by=none) → fleet-wide, and byte-identical to the
	// legacy unscoped BuildKey so existing v3 entries stay reachable.
	kNone := c.BuildScopedKey("openai", "gpt-4o", body, "", "")
	kLegacy := c.BuildKey("openai", "gpt-4o", body, "")
	if kNone != kLegacy {
		t.Fatalf("empty scope re-keyed legacy entries: %q != %q", kNone, kLegacy)
	}
	if kNone == kVKA {
		t.Fatalf("empty scope collided with vk-scoped key (no isolation): %q", kNone)
	}

	// Scope-type prefix prevents a VK id colliding with a same-valued user id.
	kUserX := c.BuildScopedKey("openai", "gpt-4o", body, "", "user:X")
	kVKX := c.BuildScopedKey("openai", "gpt-4o", body, "", "vk:X")
	if kUserX == kVKX {
		t.Fatalf("user:X and vk:X collided — scope type not folded: %q", kUserX)
	}
}

func TestBuildKey_V2_JSONKeyOrderingInvariant(t *testing.T) {
	// Same prompt with object keys in different orders must hash to
	// the same key (canonicalizeJSON sorts keys recursively).
	c := newTestCache("ai-gw:")
	bodyA := []byte(`{"a":1,"b":2}`)
	bodyB := []byte(`{"b":2,"a":1}`)
	if c.BuildKey("openai", "gpt-4o", bodyA, "") != c.BuildKey("openai", "gpt-4o", bodyB, "") {
		t.Fatal("JSON key ordering must not affect cache key")
	}
}

func TestBuildKey_V2_NestedKeyOrderingInvariant(t *testing.T) {
	// Recursive sort: nested objects also have their keys sorted.
	c := newTestCache("ai-gw:")
	bodyA := []byte(`{"messages":[{"role":"user","content":"hi"}],"model":"gpt-4o"}`)
	bodyB := []byte(`{"model":"gpt-4o","messages":[{"content":"hi","role":"user"}]}`)
	if c.BuildKey("openai", "gpt-4o", bodyA, "") != c.BuildKey("openai", "gpt-4o", bodyB, "") {
		t.Fatal("nested object key ordering must not affect cache key")
	}
}

func TestBuildKey_V2_StreamFieldRetained(t *testing.T) {
	// Stream and non-stream bodies must hash differently — the stream
	// field is intentionally retained per design D4.
	c := newTestCache("ai-gw:")
	streamBody := []byte(`{"messages":[{"role":"user","content":"hi"}],"model":"gpt-4o","stream":true}`)
	nonStreamBody := []byte(`{"messages":[{"role":"user","content":"hi"}],"model":"gpt-4o","stream":false}`)
	if c.BuildKey("openai", "gpt-4o", streamBody, "") == c.BuildKey("openai", "gpt-4o", nonStreamBody, "") {
		t.Fatal("stream and non-stream bodies must produce different keys")
	}
}

func TestBuildKey_V2_NonJSONBodyHashesDeterministically(t *testing.T) {
	// A non-JSON body (e.g. Bedrock binary) must still hash deterministically.
	c := newTestCache("ai-gw:")
	binBody := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0xFF, 0x42}
	k1 := c.BuildKey("bedrock", "anthropic.claude-3-5-sonnet", binBody, "")
	k2 := c.BuildKey("bedrock", "anthropic.claude-3-5-sonnet", binBody, "")
	if k1 != k2 {
		t.Fatal("non-JSON body must hash deterministically")
	}
	if k1 == "" {
		t.Fatal("non-JSON body produced empty key")
	}
}

func TestBuildKey_V2_ArrayElementOrderingMattersInPrompts(t *testing.T) {
	// Array element order MUST still affect the key — only object keys
	// are canonicalised, not array element order. messages = [user, assistant]
	// is not the same conversation as [assistant, user].
	c := newTestCache("ai-gw:")
	bodyA := []byte(`{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`)
	bodyB := []byte(`{"messages":[{"role":"assistant","content":"b"},{"role":"user","content":"a"}]}`)
	if c.BuildKey("openai", "gpt-4o", bodyA, "") == c.BuildKey("openai", "gpt-4o", bodyB, "") {
		t.Fatal("array element order must affect cache key")
	}
}

// StreamEntry / ResponseEntry round-trip tests

func TestStreamEntry_RoundTrip(t *testing.T) {
	c := newTestCacheWithRedis(t)
	ctx := context.Background()
	key := c.prefix + ":stream:1"

	entry := &StreamEntry{
		Provider: "openai",
		Model:    "gpt-4o",
		Chunks: []ChunkRecord{
			{Delta: "hello "},
			{Delta: "world"},
			{Done: true, Usage: &provcore.Usage{
				PromptTokens:     intPtr(5),
				CompletionTokens: intPtr(2),
			}},
		},
		Usage:    provcore.Usage{PromptTokens: intPtr(5), CompletionTokens: intPtr(2)},
		CachedAt: time.Now().UTC(),
	}
	if _, err := c.StoreStream(ctx, key, entry); err != nil {
		t.Fatalf("StoreStream: %v", err)
	}
	got := c.LookupStream(ctx, key)
	if got == nil {
		t.Fatal("LookupStream returned nil")
	}
	if got.Provider != "openai" || got.Model != "gpt-4o" {
		t.Fatalf("provider/model mismatch: %+v", got)
	}
	if len(got.Chunks) != 3 {
		t.Fatalf("chunks count mismatch: got %d", len(got.Chunks))
	}
	if got.Chunks[0].Delta != "hello " || got.Chunks[1].Delta != "world" {
		t.Fatalf("chunk content mismatch: %+v", got.Chunks)
	}
	if !got.Chunks[2].Done {
		t.Fatal("terminal chunk Done not set")
	}
	if got.Schema != schemaStream {
		t.Fatalf("schema discriminator missing or wrong: %q", got.Schema)
	}
}

func TestResponseEntry_RoundTrip(t *testing.T) {
	c := newTestCacheWithRedis(t)
	ctx := context.Background()
	key := c.prefix + ":response:1"

	entry := &ResponseEntry{
		Provider:          "openai",
		Model:             "gpt-4o",
		CanonicalResponse: json.RawMessage(`{"id":"chatcmpl-abc","choices":[{"message":{"role":"assistant","content":"hi"}}]}`),
		Usage:             provcore.Usage{PromptTokens: intPtr(5), CompletionTokens: intPtr(1)},
		CachedAt:          time.Now().UTC(),
	}
	if _, err := c.StoreResponse(ctx, key, entry); err != nil {
		t.Fatalf("StoreResponse: %v", err)
	}
	got := c.LookupResponse(ctx, key)
	if got == nil {
		t.Fatal("LookupResponse returned nil")
	}
	if !bytes.Equal(got.CanonicalResponse, entry.CanonicalResponse) {
		t.Fatalf("response body mismatch:\n got=%s\nwant=%s", got.CanonicalResponse, entry.CanonicalResponse)
	}
	if got.Schema != schemaResponse {
		t.Fatalf("schema discriminator missing or wrong: %q", got.Schema)
	}
}

func TestLookup_WrongKindReturnsNil(t *testing.T) {
	c := newTestCacheWithRedis(t)
	ctx := context.Background()

	respKey := c.prefix + ":resp:wrong-lookup"
	if _, err := c.StoreResponse(ctx, respKey, &ResponseEntry{
		Provider:          "openai",
		Model:             "gpt-4o",
		CanonicalResponse: json.RawMessage(`{}`),
		CachedAt:          time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if got := c.LookupStream(ctx, respKey); got != nil {
		t.Fatal("LookupStream against a ResponseEntry must return nil (wrong schema discriminator)")
	}

	streamKey := c.prefix + ":stream:wrong-lookup"
	if _, err := c.StoreStream(ctx, streamKey, &StreamEntry{
		Provider: "openai",
		Model:    "gpt-4o",
		Chunks:   []ChunkRecord{{Done: true}},
		CachedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if got := c.LookupResponse(ctx, streamKey); got != nil {
		t.Fatal("LookupResponse against a StreamEntry must return nil")
	}
}

func TestStoreStream_OversizeRefusedAtStore(t *testing.T) {
	c := newTestCacheWithRedis(t)
	c.maxEntryBytes = 1024 // tighten the cap for this test
	ctx := context.Background()

	big := make([]byte, 2*1024)
	for i := range big {
		big[i] = 'A'
	}
	entry := &StreamEntry{
		Provider: "openai",
		Model:    "gpt-4o",
		Chunks:   []ChunkRecord{{Delta: string(big)}},
		CachedAt: time.Now().UTC(),
	}
	_, err := c.StoreStream(ctx, c.prefix+":stream:big", entry)
	if !errors.Is(err, ErrCacheEntryTooLarge) {
		t.Fatalf("expected ErrCacheEntryTooLarge, got %v", err)
	}
}

// New() constructor — disabled paths + defaults

// TestNew_DisabledReturnsNil verifies the two short-circuits in the
// constructor contract:
//   - rdb=nil → returns nil (no redis = no cache wired)
//   - rdb!=nil + Enabled=false → returns non-nil Cache with IsEnabled()=false
//     so configdispatch can later flip it via SetConfig. All hot-path methods
//     early-return on the disabled flag, so this is equivalent to "no cache"
//     until a Hub push enables it.
func TestNew_DisabledReturnsConfiguredButOff(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mini.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer func() { _ = rdb.Close() }()

	c := New(rdb, Config{Enabled: false}, logger)
	if c == nil {
		t.Fatal("Enabled=false with rdb → want non-nil Cache (hot-swappable), got nil")
	}
	if c.IsEnabled() {
		t.Fatal("Enabled=false → IsEnabled() should be false")
	}

	// Enabled=true with nil rdb: still must return nil (no backend = feature off).
	if got := New(nil, Config{Enabled: true}, logger); got != nil {
		t.Fatalf("rdb=nil → want nil, got %+v", got)
	}
}

// TestNew_AppliesDefaults exercises the three "if zero then default"
// branches: TTL≤0 → 1h, Prefix=="" → "nexus:cache", MaxEntryBytes≤0
// → 1 MiB. We observe via the only behaviour-visible knob — the
// prefix surfaces in BuildKey output, and oversize behaviour gates
// at maxEntryBytes; TTL is observed via miniredis TTL().
func TestNew_AppliesDefaults(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mini.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer func() { _ = rdb.Close() }()

	c := New(rdb, Config{Enabled: true /* TTL, Prefix, MaxEntryBytes all zero */}, logger)
	if c == nil {
		t.Fatal("New returned nil for enabled cache")
	}

	// Prefix default.
	key := c.BuildKey("openai", "gpt-4o", []byte(`{}`), "")
	if !strings.HasPrefix(key, defaultPrefix+":") {
		t.Fatalf("default prefix not applied: %q", key)
	}

	// TTL default — write something and verify miniredis TTL ≈ 1h.
	ctx := context.Background()
	if _, err := c.StoreResponse(ctx, key, &ResponseEntry{
		Provider: "openai", Model: "gpt-4o",
		CanonicalResponse: json.RawMessage(`{}`),
		CachedAt:          time.Now().UTC(),
	}); err != nil {
		t.Fatalf("StoreResponse: %v", err)
	}
	ttl := mini.TTL(key)
	if ttl < 59*time.Minute || ttl > time.Hour+time.Minute {
		t.Fatalf("default TTL not ~1h: got %v", ttl)
	}

	// MaxEntryBytes default — value should be 1<<20.
	if c.maxEntryBytes != 1<<20 {
		t.Fatalf("default maxEntryBytes not 1MiB: %d", c.maxEntryBytes)
	}
}

// Nil-receiver no-op contract

// TestNilReceiver_AllMethodsAreNoOps locks in the documented "nil *Cache
// is safe" contract — the caller chain (proxy_cache.go) relies on this
// to avoid `if cache != nil` guards at every call site.
func TestNilReceiver_AllMethodsAreNoOps(t *testing.T) {
	var c *Cache
	ctx := context.Background()

	// Store paths: return (0, nil) without panicking.
	if n, err := c.StoreStream(ctx, "k", &StreamEntry{}); n != 0 || err != nil {
		t.Fatalf("nil StoreStream: got (%d,%v), want (0,nil)", n, err)
	}
	if n, err := c.StoreResponse(ctx, "k", &ResponseEntry{}); n != 0 || err != nil {
		t.Fatalf("nil StoreResponse: got (%d,%v), want (0,nil)", n, err)
	}

	// Lookup paths: return nil without panicking.
	if got := c.LookupStream(ctx, "k"); got != nil {
		t.Fatalf("nil LookupStream: got %+v, want nil", got)
	}
	if got := c.LookupResponse(ctx, "k"); got != nil {
		t.Fatalf("nil LookupResponse: got %+v, want nil", got)
	}

	// Flush returns (0, nil).
	if n, err := c.Flush(ctx, "openai"); n != 0 || err != nil {
		t.Fatalf("nil Flush: got (%d,%v), want (0,nil)", n, err)
	}

	// Stats returns three zeros.
	if h, m, s := c.Stats(ctx); h != 0 || m != 0 || s != 0 {
		t.Fatalf("nil Stats: got (%d,%d,%d), want (0,0,0)", h, m, s)
	}

	// RecordHit / RecordMiss: must not panic.
	c.RecordHit(ctx)
	c.RecordMiss(ctx)
}

// TestStore_EmptyKeyAndNilEntryAreNoOps covers the second short-circuit
// in StoreStream / StoreResponse: a non-nil *Cache but a missing key
// or nil entry must skip the write silently. We assert nothing was
// written to Redis to confirm the no-op semantics (not just the
// return value).
func TestStore_EmptyKeyAndNilEntryAreNoOps(t *testing.T) {
	c := newTestCacheWithRedis(t)
	ctx := context.Background()

	if n, err := c.StoreStream(ctx, "", &StreamEntry{}); n != 0 || err != nil {
		t.Fatalf("empty key StoreStream: got (%d,%v)", n, err)
	}
	if n, err := c.StoreStream(ctx, c.prefix+":k", nil); n != 0 || err != nil {
		t.Fatalf("nil entry StoreStream: got (%d,%v)", n, err)
	}
	if n, err := c.StoreResponse(ctx, "", &ResponseEntry{}); n != 0 || err != nil {
		t.Fatalf("empty key StoreResponse: got (%d,%v)", n, err)
	}
	if n, err := c.StoreResponse(ctx, c.prefix+":k", nil); n != 0 || err != nil {
		t.Fatalf("nil entry StoreResponse: got (%d,%v)", n, err)
	}

	// Lookup of an empty key must also short-circuit before hitting Redis.
	if got := c.LookupStream(ctx, ""); got != nil {
		t.Fatalf("empty key LookupStream returned non-nil")
	}
	if got := c.LookupResponse(ctx, ""); got != nil {
		t.Fatalf("empty key LookupResponse returned non-nil")
	}
}

// Lookup miss / error paths

// TestLookup_MissReturnsNil — redis.Nil error must not log, must
// return nil. The "miss" branch is the hot path.
func TestLookup_MissReturnsNil(t *testing.T) {
	c := newTestCacheWithRedis(t)
	ctx := context.Background()

	if got := c.LookupStream(ctx, c.prefix+":no-such-key"); got != nil {
		t.Fatalf("LookupStream on missing key: got %+v, want nil", got)
	}
	if got := c.LookupResponse(ctx, c.prefix+":no-such-key"); got != nil {
		t.Fatalf("LookupResponse on missing key: got %+v, want nil", got)
	}
}

// TestLookup_RedisErrorReturnsNil covers the non-redis.Nil error
// branch — a Redis-level failure (timeout, connection drop) must
// surface as a miss, not propagate. Models the AI gateway's
// "cache failures degrade to passthrough" requirement.
func TestLookup_RedisErrorReturnsNil(t *testing.T) {
	s, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	c := New(rdb, Config{Enabled: true, TTL: time.Minute, Prefix: "errprobe"}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Inject an error from miniredis — every subsequent command returns it.
	s.SetError("forced GET failure")
	ctx := context.Background()

	if got := c.LookupStream(ctx, "errprobe:k"); got != nil {
		t.Fatalf("LookupStream under redis error: got %+v, want nil", got)
	}
	if got := c.LookupResponse(ctx, "errprobe:k"); got != nil {
		t.Fatalf("LookupResponse under redis error: got %+v, want nil", got)
	}
}

// TestLookup_ProbeDecodeErrorReturnsNil — when the stored value is
// not valid JSON, the schema-probe Unmarshal fails and Lookup must
// return nil rather than crash or surface bad data.
func TestLookup_ProbeDecodeErrorReturnsNil(t *testing.T) {
	c := newTestCacheWithRedis(t)
	ctx := context.Background()

	key := c.prefix + ":garbage"
	// Write raw non-JSON bytes directly via the rdb handle.
	if err := c.rdb.Set(ctx, key, []byte("not-json-{garbage"), c.ttl()).Err(); err != nil {
		t.Fatal(err)
	}
	if got := c.LookupStream(ctx, key); got != nil {
		t.Fatalf("LookupStream on garbage: want nil, got %+v", got)
	}
	if got := c.LookupResponse(ctx, key); got != nil {
		t.Fatalf("LookupResponse on garbage: want nil, got %+v", got)
	}
}

// TestLookup_FullDecodeErrorReturnsNil — schema discriminator matches
// the expected kind, but the full entry shape is malformed. The
// second-stage Unmarshal must fail and Lookup must return nil.
//
// This is the failure mode where a future field rename or type
// change could surface stale entries; the strict decode + nil-return
// keeps callers safe.
func TestLookup_FullDecodeErrorReturnsNil(t *testing.T) {
	c := newTestCacheWithRedis(t)
	ctx := context.Background()

	// Schema=stream/v1 but Chunks is a string instead of an array.
	streamKey := c.prefix + ":bad-stream"
	if err := c.rdb.Set(ctx, streamKey,
		[]byte(`{"schema":"stream/v1","chunks":"not-an-array"}`),
		c.ttl()).Err(); err != nil {
		t.Fatal(err)
	}
	if got := c.LookupStream(ctx, streamKey); got != nil {
		t.Fatalf("LookupStream on shape-mismatched stream entry: want nil, got %+v", got)
	}

	// Schema=response/v1 but response is a number instead of a JSON value
	// (json.RawMessage accepts anything valid, so use usage as the
	// shape-breaker — non-object where struct is expected).
	respKey := c.prefix + ":bad-response"
	if err := c.rdb.Set(ctx, respKey,
		[]byte(`{"schema":"response/v1","usage":"not-an-object"}`),
		c.ttl()).Err(); err != nil {
		t.Fatal(err)
	}
	if got := c.LookupResponse(ctx, respKey); got != nil {
		t.Fatalf("LookupResponse on shape-mismatched response entry: want nil, got %+v", got)
	}
}

// Store error paths

// TestStoreResponse_MarshalErrorSurfaces — a ResponseEntry with an
// invalid json.RawMessage triggers json.Marshal failure. The error
// is returned (not swallowed), n == 0, and Redis is untouched.
func TestStoreResponse_MarshalErrorSurfaces(t *testing.T) {
	c := newTestCacheWithRedis(t)
	ctx := context.Background()

	// Invalid RawMessage — `{invalid}` triggers MarshalJSON error.
	entry := &ResponseEntry{
		Provider:          "openai",
		Model:             "gpt-4o",
		CanonicalResponse: json.RawMessage(`{invalid-json}`),
		CachedAt:          time.Now().UTC(),
	}
	key := c.prefix + ":bad-marshal"
	n, err := c.StoreResponse(ctx, key, entry)
	if err == nil {
		t.Fatal("StoreResponse with invalid RawMessage: want marshal error, got nil")
	}
	if n != 0 {
		t.Fatalf("StoreResponse marshal error: want n=0, got %d", n)
	}
	// Confirm nothing was written.
	if got := c.LookupResponse(ctx, key); got != nil {
		t.Fatal("StoreResponse marshal error left a value in Redis")
	}
}

// TestStoreResponse_OversizeRefusedAtStore — mirror of the existing
// stream oversize test for the response path; the size cap must be
// enforced uniformly across both schemas.
func TestStoreResponse_OversizeRefusedAtStore(t *testing.T) {
	c := newTestCacheWithRedis(t)
	c.maxEntryBytes = 256
	ctx := context.Background()

	big := bytes.Repeat([]byte(`"abcdefgh",`), 200) // > 256 bytes after wrapping
	body := []byte(`[` + string(big[:len(big)-1]) + `]`)
	entry := &ResponseEntry{
		Provider:          "openai",
		Model:             "gpt-4o",
		CanonicalResponse: json.RawMessage(body),
		CachedAt:          time.Now().UTC(),
	}
	n, err := c.StoreResponse(ctx, c.prefix+":response:big", entry)
	if !errors.Is(err, ErrCacheEntryTooLarge) {
		t.Fatalf("StoreResponse oversize: want ErrCacheEntryTooLarge, got %v", err)
	}
	if n <= c.maxEntryBytes {
		t.Fatalf("oversize n must report serialised size > cap; got n=%d cap=%d", n, c.maxEntryBytes)
	}
}

// TestStore_RedisSetErrorSurfaces — a SET-level Redis failure must
// surface as an error from StoreStream / StoreResponse (callers in
// proxy_cache.go log and continue, but the error must be observable
// so the warning fires).
func TestStore_RedisSetErrorSurfaces(t *testing.T) {
	s, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	c := New(rdb, Config{Enabled: true, TTL: time.Minute, Prefix: "seterr"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	s.SetError("SET refused")

	if _, err := c.StoreStream(ctx, "seterr:s", &StreamEntry{
		Provider: "openai", Model: "gpt-4o",
		Chunks:   []ChunkRecord{{Done: true}},
		CachedAt: time.Now().UTC(),
	}); err == nil {
		t.Fatal("StoreStream under SET error: want error, got nil")
	}
	if _, err := c.StoreResponse(ctx, "seterr:r", &ResponseEntry{
		Provider: "openai", Model: "gpt-4o",
		CanonicalResponse: json.RawMessage(`{}`),
		CachedAt:          time.Now().UTC(),
	}); err == nil {
		t.Fatal("StoreResponse under SET error: want error, got nil")
	}
}

// TTL expiry — observable cache-miss after TTL

// TestStore_TTLExpiryProducesMiss — after the configured TTL elapses,
// Lookup must return nil. miniredis.FastForward simulates wall-clock
// advance deterministically.
func TestStore_TTLExpiryProducesMiss(t *testing.T) {
	s, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	c := New(rdb, Config{Enabled: true, TTL: 30 * time.Second, Prefix: "ttl"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	key := "ttl:expiry"
	if _, err := c.StoreResponse(ctx, key, &ResponseEntry{
		Provider: "openai", Model: "gpt-4o",
		CanonicalResponse: json.RawMessage(`{"x":1}`),
		CachedAt:          time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	// Pre-expiry: must HIT.
	if got := c.LookupResponse(ctx, key); got == nil {
		t.Fatal("pre-TTL: want HIT, got nil")
	}
	// Advance past TTL.
	s.FastForward(31 * time.Second)
	if got := c.LookupResponse(ctx, key); got != nil {
		t.Fatal("post-TTL: want MISS, got HIT")
	}
}

// Flush — provider-scoped and unscoped

// TestFlush_DeletesAllEntriesUnderPrefix — Flush is currently
// prefix-scoped (provider arg accepted but ignored per code comment).
// Both flavours must wipe everything under the prefix and return
// the deletion count.
func TestFlush_DeletesAllEntriesUnderPrefix(t *testing.T) {
	c := newTestCacheWithRedis(t)
	ctx := context.Background()

	// Seed: 3 response entries + 2 stream entries.
	for i := range 3 {
		if _, err := c.StoreResponse(ctx, fmt.Sprintf("%s:r:%d", c.prefix, i),
			&ResponseEntry{Provider: "openai", Model: "gpt-4o",
				CanonicalResponse: json.RawMessage(`{}`),
				CachedAt:          time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 2 {
		if _, err := c.StoreStream(ctx, fmt.Sprintf("%s:s:%d", c.prefix, i),
			&StreamEntry{Provider: "openai", Model: "gpt-4o",
				Chunks:   []ChunkRecord{{Done: true}},
				CachedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}

	// Empty provider — flush everything.
	deleted, err := c.Flush(ctx, "")
	if err != nil {
		t.Fatalf("Flush(\"\"): %v", err)
	}
	if deleted != 5 {
		t.Fatalf("Flush deleted=%d, want 5", deleted)
	}
	// All entries must be gone.
	for i := range 3 {
		if got := c.LookupResponse(ctx, fmt.Sprintf("%s:r:%d", c.prefix, i)); got != nil {
			t.Fatalf("entry %d survived flush", i)
		}
	}

	// Provider arg is accepted but ignored — confirm flushing under a
	// specific provider still wipes everything (current behaviour per
	// comment in Flush). Add some entries again and check.
	if _, err := c.StoreResponse(ctx, c.prefix+":x",
		&ResponseEntry{CanonicalResponse: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	deleted2, err := c.Flush(ctx, "openai")
	if err != nil {
		t.Fatalf("Flush(\"openai\"): %v", err)
	}
	if deleted2 != 1 {
		t.Fatalf("provider-scoped Flush deleted=%d, want 1 (provider arg ignored)", deleted2)
	}

	// Flush of empty namespace returns 0 cleanly.
	deleted3, err := c.Flush(ctx, "")
	if err != nil {
		t.Fatalf("empty Flush: %v", err)
	}
	if deleted3 != 0 {
		t.Fatalf("empty Flush deleted=%d, want 0", deleted3)
	}
}

// TestFlush_ScanErrorSurfaces — a Redis-level failure during SCAN must
// propagate (wrapped) so operators see "cache flush scan: ...".
func TestFlush_ScanErrorSurfaces(t *testing.T) {
	s, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	c := New(rdb, Config{Enabled: true, TTL: time.Minute, Prefix: "flusherr"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	s.SetError("forced SCAN failure")
	_, err = c.Flush(ctx, "")
	if err == nil {
		t.Fatal("Flush under SCAN error: want error, got nil")
	}
	if !strings.Contains(err.Error(), "cache flush") {
		t.Fatalf("error not wrapped: %v", err)
	}
}

// TestFlush_DelErrorSurfaces — SCAN succeeds (returns keys) but DEL
// fails. The Flush function wraps with "cache flush del: ...".
//
// miniredis.SetError applies to every command, so to exercise the
// "scan ok / del fails" branch we toggle the error AFTER the seed
// scan. Since miniredis returns the cursor in one shot for our key
// count, we instead use a single-command toggle: SetError applies to
// the next command — so we seed without error, then set the error
// and call Flush, which will fail on its SCAN. That's already covered
// above. The DEL-error path is therefore reachable only via
// fine-grained command interception (not supported by miniredis);
// document it as an intentional gap.
//
// Note: marshalSortedKeys error returns (3 defensive lines), New's
// disabled-when-nil-rdb branch (covered), the StoreStream
// marshal-error line, and the canonicalizeJSON "marshalSortedKeys
// err" return are all defensive code with no reachable input shape
// through the public API plus json.Unmarshal's output shape — they
// stay uncovered, but the 95% target is met by the surrounding
// behaviour tests above.

// Stats / RecordHit / RecordMiss

// TestStats_CountsHitsMissesAndEntries verifies the three numbers
// returned by Stats: hit counter (INCR'd by RecordHit), miss counter
// (INCR'd by RecordMiss), and current entry count under the prefix
// (excluding the two stats sidecar keys themselves).
func TestStats_CountsHitsMissesAndEntries(t *testing.T) {
	c := newTestCacheWithRedis(t)
	ctx := context.Background()

	// 3 hits, 2 misses, 4 entries.
	for range 3 {
		c.RecordHit(ctx)
	}
	for range 2 {
		c.RecordMiss(ctx)
	}
	for i := range 4 {
		if _, err := c.StoreResponse(ctx, fmt.Sprintf("%s:r:%d", c.prefix, i),
			&ResponseEntry{Provider: "openai", Model: "gpt-4o",
				CanonicalResponse: json.RawMessage(`{}`),
				CachedAt:          time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}

	hits, misses, size := c.Stats(ctx)
	if hits != 3 {
		t.Fatalf("hits=%d, want 3", hits)
	}
	if misses != 2 {
		t.Fatalf("misses=%d, want 2", misses)
	}
	if size != 4 {
		t.Fatalf("size=%d, want 4 (stats keys must be excluded)", size)
	}
}

// TestStats_EmptyPrefixReturnsZeros — fresh cache, no stats keys
// written, no entries: all three values are zero (Get on absent
// counter key returns 0 via the swallowed error).
func TestStats_EmptyPrefixReturnsZeros(t *testing.T) {
	c := newTestCacheWithRedis(t)
	ctx := context.Background()

	hits, misses, size := c.Stats(ctx)
	if hits != 0 || misses != 0 || size != 0 {
		t.Fatalf("empty cache Stats: got (%d,%d,%d), want (0,0,0)", hits, misses, size)
	}
}

// TestStats_ScanErrorBreaksCountLoop — when SCAN fails mid-iteration,
// Stats returns whatever counters were read (which still come from
// the same SetError-guarded GET, so will be 0) plus a partial size.
// The function must not block forever and must not panic; size
// reflects "best effort" partial count.
func TestStats_ScanErrorBreaksCountLoop(t *testing.T) {
	s, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	c := New(rdb, Config{Enabled: true, TTL: time.Minute, Prefix: "statserr"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	s.SetError("forced failure")

	// Must not panic / hang; values are best-effort.
	hits, misses, size := c.Stats(ctx)
	if hits != 0 || misses != 0 || size != 0 {
		t.Fatalf("Stats under error: got (%d,%d,%d), want (0,0,0)", hits, misses, size)
	}
}

// TestRecordHitMiss_PersistedInRedis verifies that RecordHit /
// RecordMiss write to deterministic sidecar keys (so Stats reads
// the same value the recorder wrote).
func TestRecordHitMiss_PersistedInRedis(t *testing.T) {
	c := newTestCacheWithRedis(t)
	ctx := context.Background()

	c.RecordHit(ctx)
	c.RecordHit(ctx)
	c.RecordMiss(ctx)

	gotHits, err := c.rdb.Get(ctx, c.prefix+":stats:hits").Int64()
	if err != nil {
		t.Fatal(err)
	}
	if gotHits != 2 {
		t.Fatalf("stats:hits=%d, want 2", gotHits)
	}
	gotMisses, err := c.rdb.Get(ctx, c.prefix+":stats:misses").Int64()
	if err != nil {
		t.Fatal(err)
	}
	if gotMisses != 1 {
		t.Fatalf("stats:misses=%d, want 1", gotMisses)
	}
}

// canonicalizeJSON / marshalSortedKeys edge cases

// TestCanonicalizeJSON_InvalidJSONPassedThrough — invalid JSON is
// returned unchanged so BuildKey still hashes deterministically.
// This is the explicit Unmarshal-error branch.
func TestCanonicalizeJSON_InvalidJSONPassedThrough(t *testing.T) {
	bad := []byte(`{not json}`)
	got := canonicalizeJSON(bad)
	if !bytes.Equal(got, bad) {
		t.Fatalf("invalid JSON not passed through: got %q, want %q", got, bad)
	}
}

// TestCanonicalizeJSON_PrimitivesAndArraysHandled covers the
// non-object branches of marshalSortedKeys: top-level scalar, array
// of scalars, deeply nested arrays.
func TestCanonicalizeJSON_PrimitivesAndArraysHandled(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`42`, `42`},
		{`"hello"`, `"hello"`},
		{`null`, `null`},
		{`true`, `true`},
		{`[3,1,2]`, `[3,1,2]`}, // array order preserved
		{`[{"b":2,"a":1}]`, `[{"a":1,"b":2}]`},
		{`{"z":[{"y":1,"x":2}],"a":3}`, `{"a":3,"z":[{"x":2,"y":1}]}`},
	}
	for _, tc := range cases {
		got := canonicalizeJSON([]byte(tc.in))
		if string(got) != tc.want {
			t.Errorf("canonicalizeJSON(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Cache hit identity — body equivalence on round-trip

// TestStreamEntry_HITBytesIdenticalToMISS is the strongest behavioural
// guarantee of the response cache: a HIT must replay byte-for-byte
// what the upstream produced at MISS. The RawBytes per-chunk preserve
// the SSE/NDJSON framing exactly.
func TestStreamEntry_HITBytesIdenticalToMISS(t *testing.T) {
	c := newTestCacheWithRedis(t)
	ctx := context.Background()

	// Simulate three SSE frames with realistic OpenAI shape.
	frame1 := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n")
	frame2 := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\n")
	frame3 := []byte("data: [DONE]\n\n")

	in := &StreamEntry{
		Provider: "openai",
		Model:    "gpt-4o",
		Chunks: []ChunkRecord{
			{Delta: "hello ", RawBytes: frame1},
			{Delta: "world", RawBytes: frame2},
			{Done: true, RawBytes: frame3},
		},
		Usage:    provcore.Usage{PromptTokens: intPtr(5), CompletionTokens: intPtr(2)},
		CachedAt: time.Now().UTC(),
		UpstreamHeaders: map[string][]string{
			"Content-Type":          {"text/event-stream"},
			"X-Ratelimit-Remaining": {"9999"},
			"Openai-Processing-Ms":  {"123"},
		},
	}
	key := c.prefix + ":hit-identity"
	if _, err := c.StoreStream(ctx, key, in); err != nil {
		t.Fatal(err)
	}

	out := c.LookupStream(ctx, key)
	if out == nil {
		t.Fatal("HIT returned nil")
	}
	if len(out.Chunks) != 3 {
		t.Fatalf("chunk count drift: %d", len(out.Chunks))
	}
	if !bytes.Equal(out.Chunks[0].RawBytes, frame1) {
		t.Fatalf("frame1 corrupted on HIT replay")
	}
	if !bytes.Equal(out.Chunks[1].RawBytes, frame2) {
		t.Fatalf("frame2 corrupted on HIT replay")
	}
	if !bytes.Equal(out.Chunks[2].RawBytes, frame3) {
		t.Fatalf("frame3 corrupted on HIT replay")
	}
	if !out.Chunks[2].Done {
		t.Fatal("terminal Done flag lost")
	}
	if got, want := out.UpstreamHeaders["Content-Type"], []string{"text/event-stream"}; !equalStrings(got, want) {
		t.Fatalf("upstream headers Content-Type lost: %v", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func intPtr(n int) *int { return &n }

// Coverage for the runtime-swappable Config seam.
// Hot-path consumers (proxy_cache.go classifyCachePreLookup) read
// SetConfig / IsEnabled / ApplyFreshnessRules without holding a lock,
// so we pin the atomic-load semantics + the nil-receiver no-op contract.

func TestSetConfig_HotSwaps(t *testing.T) {
	c := newTestCacheWithRedis(t)
	if !c.IsEnabled() {
		t.Fatal("initial IsEnabled = false; want true")
	}
	if c.ApplyFreshnessRules() {
		t.Fatal("initial ApplyFreshnessRules = true; want false")
	}
	if got := c.ttl(); got != time.Minute {
		t.Fatalf("initial ttl = %v; want 1m", got)
	}

	c.SetConfig(ConfigSnapshot{Enabled: false, TTL: 5 * time.Minute, ApplyFreshnessRules: true})
	if c.IsEnabled() {
		t.Fatal("post-SetConfig IsEnabled = true; want false")
	}
	if !c.ApplyFreshnessRules() {
		t.Fatal("post-SetConfig ApplyFreshnessRules = false; want true")
	}
	if got := c.ttl(); got != 5*time.Minute {
		t.Fatalf("post-SetConfig ttl = %v; want 5m", got)
	}
}

func TestSetConfig_ZeroTTLPreservesExisting(t *testing.T) {
	c := newTestCacheWithRedis(t) // sets up TTL = 1 minute
	c.SetConfig(ConfigSnapshot{Enabled: true, TTL: 0, ApplyFreshnessRules: false})
	if got := c.ttl(); got != time.Minute {
		t.Fatalf("zero-TTL should preserve existing 1m; got %v", got)
	}
}

func TestNilReceiverSafe(t *testing.T) {
	var c *Cache
	c.SetConfig(ConfigSnapshot{Enabled: true, TTL: time.Minute})
	if c.IsEnabled() {
		t.Fatal("nil cache IsEnabled must be false")
	}
	if c.ApplyFreshnessRules() {
		t.Fatal("nil cache ApplyFreshnessRules must be false")
	}
	if got := c.ttl(); got != 0 {
		t.Fatalf("nil cache ttl must be 0; got %v", got)
	}
}
