package jwtverifier

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// defaultJWKSTTL is the cache lifetime for a successfully fetched JWKS
// snapshot. Within the TTL, KeyByKID serves from memory; past it, the next
// caller triggers a refresh. On refresh failure within TTL, the cached
// snapshot is returned (stale-while-revalidate).
const defaultJWKSTTL = 15 * time.Minute

// defaultJWKSHTTPTimeout is the per-request timeout used by the default
// HTTP client.
const defaultJWKSHTTPTimeout = 5 * time.Second

// JWKSCache fetches and caches RSA public keys from a JWKS endpoint. It is
// safe for concurrent use. Concurrent refreshes are coalesced through a
// singleflight group so a stampede on a cold cache only hits the upstream
// once.
type JWKSCache struct {
	url   string
	httpc *http.Client
	ttl   time.Duration

	mu  sync.RWMutex
	cur *jwksEntry

	sf singleflight.Group
}

// jwksEntry is a point-in-time snapshot of the keys parsed from one
// successful JWKS response.
type jwksEntry struct {
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

// NewJWKSCache returns a cache that fetches from url with the default
// 5-second HTTP timeout and 15-minute TTL.
func NewJWKSCache(url string) *JWKSCache {
	return NewJWKSCacheWithTTL(url, defaultJWKSTTL)
}

// NewJWKSCacheWithTTL is like NewJWKSCache but lets the caller pick a
// non-default TTL. Intended for tests that do not want to wait 15 minutes
// to exercise the stale-while-revalidate behavior.
func NewJWKSCacheWithTTL(url string, ttl time.Duration) *JWKSCache {
	return &JWKSCache{
		url: url,
		httpc: nexushttp.New(nexushttp.Config{
			Timeout:        defaultJWKSHTTPTimeout,
			Caller:         "cp-jwt-jwks",
			PropagateReqID: true,
		}),
		ttl: ttl,
	}
}

// KeyByKID returns the RSA public key whose JWKS kid matches. If the cache
// is fresh and holds the kid, it is served from memory. Otherwise a
// refresh is attempted; on transient failure a still-within-TTL cached key
// is returned (stale-while-revalidate). If neither fresh nor stale has the
// kid, ErrJWKSUnavailable is returned.
func (c *JWKSCache) KeyByKID(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	c.mu.RLock()
	cur := c.cur
	c.mu.RUnlock()

	if cur != nil && time.Since(cur.fetchedAt) < c.ttl {
		if k, ok := cur.keys[kid]; ok {
			return k, nil
		}
	}

	_, err, _ := c.sf.Do("fetch", func() (any, error) {
		return nil, c.refresh(ctx)
	})
	if err != nil {
		// Serve stale if still within TTL and kid present.
		if cur != nil && time.Since(cur.fetchedAt) < c.ttl {
			if k, ok := cur.keys[kid]; ok {
				return k, nil
			}
		}
		return nil, ErrJWKSUnavailable
	}

	c.mu.RLock()
	cur = c.cur
	c.mu.RUnlock()
	if cur == nil {
		return nil, ErrJWKSUnavailable
	}
	if k, ok := cur.keys[kid]; ok {
		return k, nil
	}
	return nil, ErrJWKSUnavailable
}

// jwksDoc is the wire shape of an RFC 7517 JWKS document, reduced to the
// fields we consume.
type jwksDoc struct {
	Keys []jwksKey `json:"keys"`
}

type jwksKey struct {
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// refresh fetches the JWKS document, parses RSA/RS256/sig keys, and swaps
// the result in under the write lock. Non-RSA or non-signing keys are
// skipped silently (valid per RFC 7517 — publishers may include key types
// we do not verify).
func (c *JWKSCache) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return fmt.Errorf("jwks: build request: %w", err)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		// Surface ctx cancellation verbatim; callers may want to distinguish
		// it from upstream failures.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("jwks: fetch: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks: status %d", resp.StatusCode)
	}

	var doc jwksDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("jwks: decode: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Alg != "RS256" || k.Use != "sig" {
			continue
		}
		if k.Kid == "" || k.N == "" || k.E == "" {
			continue
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		n := new(big.Int).SetBytes(nBytes)
		e := new(big.Int).SetBytes(eBytes)
		if !e.IsInt64() {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: n, E: int(e.Int64())}
	}

	entry := &jwksEntry{keys: keys, fetchedAt: time.Now()}
	c.mu.Lock()
	c.cur = entry
	c.mu.Unlock()
	return nil
}
