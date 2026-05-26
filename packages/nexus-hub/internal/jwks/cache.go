// Package jwks caches public keys fetched from a JWKS endpoint.
// It is used by the Hub enrollment handler to verify enrollment JWTs signed
// by the Control Plane. Keys are refreshed every 5 minutes; on refresh failure
// the stale cache is used for up to 10 minutes before being cleared.
package jwks

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"sync"
	"time"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// ErrCacheEmpty is returned when no keys have ever been successfully loaded.
// The caller should respond with 503 to indicate the JWKS endpoint has not
// been reachable since Hub started.
var ErrCacheEmpty = errors.New("jwks: cache is empty; CP JWKS endpoint may be unreachable")

const (
	refreshInterval         = 5 * time.Minute
	defaultStaleGracePeriod = 10 * time.Minute
)

type jwkJSON struct {
	KTY string `json:"kty"`
	KID string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwksJSON struct {
	Keys []jwkJSON `json:"keys"`
}

// Cache fetches RSA public keys from a JWKS endpoint, caches them by kid,
// and refreshes the cache every refreshInterval. On fetch failure the stale
// cache remains valid for staleGrace after the last successful fetch;
// beyond that the cache is cleared and Get returns ErrCacheEmpty.
type Cache struct {
	url        string
	client     *http.Client
	logger     *slog.Logger
	staleGrace time.Duration

	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time // zero until first successful fetch
	done      chan struct{}
}

// New creates a Cache that fetches from url, starts a background refresh
// goroutine, and performs an initial fetch. The background goroutine runs
// for the process lifetime; Close may be called to stop it.
func New(url string, logger *slog.Logger) *Cache {
	return newWithGrace(url, logger, defaultStaleGracePeriod)
}

// newWithGrace is the test-only seam for staleGracePeriod. Production
// callers use New, which pins the 10-minute grace per the JWKS staleness
// SLO. Tests inject a short value to drive the stale-clear branch in
// handleFetchFailure without sleeping for ten minutes.
func newWithGrace(url string, logger *slog.Logger, staleGrace time.Duration) *Cache {
	c := &Cache{
		url:        url,
		client: nexushttp.New(nexushttp.Config{
			Timeout:        10 * time.Second,
			Caller:         "hub-jwks-cache",
			PropagateReqID: true,
		}),
		logger:     logger,
		staleGrace: staleGrace,
		keys:       map[string]*rsa.PublicKey{},
		done:       make(chan struct{}),
	}
	go c.run()
	return c
}

// Close stops the background refresh goroutine.
func (c *Cache) Close() {
	close(c.done)
}

// Get returns the RSA public key for the given kid.
//
//   - If the cache is empty (never fetched), ErrCacheEmpty is returned.
//   - If kid is empty, the first key in the cache is returned (convenience
//     for single-key JWKS sets).
//   - If kid is unknown, an error naming the kid is returned.
func (c *Cache) Get(kid string) (*rsa.PublicKey, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.keys) == 0 {
		return nil, ErrCacheEmpty
	}
	if kid == "" {
		for _, k := range c.keys {
			return k, nil
		}
	}
	k, ok := c.keys[kid]
	if !ok {
		return nil, fmt.Errorf("jwks: kid %q not in cache", kid)
	}
	return k, nil
}

func (c *Cache) run() {
	// Initial fetch at startup before the ticker fires.
	c.refresh()

	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.refresh()
		}
	}
}

func (c *Cache) refresh() {
	resp, err := c.client.Get(c.url) //nolint:noctx
	if err != nil {
		c.logger.Warn("jwks: fetch failed", "url", c.url, "error", err)
		c.handleFetchFailure()
		return
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		c.logger.Warn("jwks: fetch returned non-200", "url", c.url, "status", resp.StatusCode)
		c.handleFetchFailure()
		return
	}

	var parsed jwksJSON
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		c.logger.Warn("jwks: parse error", "url", c.url, "error", err)
		c.handleFetchFailure()
		return
	}

	newKeys := make(map[string]*rsa.PublicKey, len(parsed.Keys))
	for _, k := range parsed.Keys {
		if k.KTY != "RSA" {
			continue
		}
		pub, err := parseRSAKey(k)
		if err != nil {
			c.logger.Warn("jwks: skip malformed key", "kid", k.KID, "error", err)
			continue
		}
		newKeys[k.KID] = pub
	}

	c.mu.Lock()
	c.keys = newKeys
	c.fetchedAt = time.Now()
	c.mu.Unlock()

	c.logger.Debug("jwks: refreshed", "url", c.url, "keys", len(newKeys))
}

// handleFetchFailure clears the cache when the stale grace period has elapsed.
func (c *Cache) handleFetchFailure() {
	c.mu.RLock()
	fetchedAt := c.fetchedAt
	c.mu.RUnlock()

	if !fetchedAt.IsZero() && time.Since(fetchedAt) > c.staleGrace {
		c.mu.Lock()
		c.keys = map[string]*rsa.PublicKey{}
		c.mu.Unlock()
		c.logger.Error("jwks: stale cache expired; clearing keys",
			"url", c.url, "last_fetch", fetchedAt)
	}
}

func parseRSAKey(k jwkJSON) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}

	n := new(big.Int).SetBytes(nBytes)
	e := new(big.Int).SetBytes(eBytes)

	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}
