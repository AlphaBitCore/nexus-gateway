// packages/ai-gateway/internal/policy/aiguard/cache.go
package aiguard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/goccy/go-json"
	"time"

	"github.com/redis/go-redis/v9"
)

// Cache wraps go-redis with the ai-guard-specific key prefix + JSON
// encoding. Safe for concurrent use.
type Cache struct {
	rdb redis.UniversalClient
}

// NewCache returns a Cache backed by rdb. rdb may be nil, in which case
// Get always returns miss and Set is a no-op — callers don't need to
// branch on "Redis configured?".
func NewCache(rdb redis.UniversalClient) *Cache { return &Cache{rdb: rdb} }

// CacheKey returns the canonical cache key for a classify request.
// Shape: aiguard:v1:<sha256(detector_type + "\n" + normalized + "\n" + backend_fp)>
func CacheKey(detectorType, normalizedContent, backendFingerprint string) string {
	h := sha256.New()
	h.Write([]byte(detectorType))
	h.Write([]byte{'\n'})
	h.Write([]byte(normalizedContent))
	h.Write([]byte{'\n'})
	h.Write([]byte(backendFingerprint))
	return "aiguard:v1:" + hex.EncodeToString(h.Sum(nil))
}

// Get returns the cached Response and hit=true when present. hit=false
// with nil error on miss or when rdb is nil. Unmarshal errors surface as
// cache miss + a non-nil error so callers can log and recompute.
func (c *Cache) Get(ctx context.Context, key string) (*Response, bool, error) {
	if c == nil || c.rdb == nil {
		return nil, false, nil
	}
	raw, err := c.rdb.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, false, err
	}
	return &resp, true, nil
}

// Set writes resp under key with ttl. TTL <= 0 is a no-op (cache disabled).
func (c *Cache) Set(ctx context.Context, key string, resp *Response, ttl time.Duration) error {
	if c == nil || c.rdb == nil {
		return nil
	}
	if ttl <= 0 {
		return nil
	}
	buf, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, key, buf, ttl).Err()
}
