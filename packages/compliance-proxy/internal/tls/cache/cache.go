package cache

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/tls/issuer"
)

// redisKeyPrefix is the key prefix for certificate entries in Redis. The CA
// fingerprint is appended to it (see caScope) so entries cannot outlive the CA
// that minted them.
const redisKeyPrefix = "nexus:proxy:cert:"

// redisCertEntry is the JSON structure stored in Redis for each cached certificate.
type redisCertEntry struct {
	EncryptedKey string `json:"encryptedKey"`
	CertChainPEM string `json:"certChainPEM"`
	Nonce        string `json:"nonce"`
	CreatedAt    string `json:"createdAt"`
}

// CertCache implements the two-layer certificate cache (LRU -> Redis -> Sign).
type CertCache struct {
	iss    *issuer.Issuer
	lru    *LRUCache
	redis  redis.UniversalClient // nil if Redis unavailable
	ttl    time.Duration
	logger *slog.Logger
}

// NewCertCache creates a new two-layer cert cache.
func NewCertCache(iss *issuer.Issuer, lru *LRUCache, redisClient redis.UniversalClient, ttl time.Duration, logger *slog.Logger) *CertCache {
	// Probe Redis on startup so the redis_available gauge reflects reality
	// before any TLS traffic arrives.
	if redisClient != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := redisClient.Ping(ctx).Err(); err != nil {
			if metrics.RedisAvailable != nil {
				metrics.RedisAvailable.With().Set(0)
			}
		} else if metrics.RedisAvailable != nil {
			metrics.RedisAvailable.With().Set(1)
		}
	}

	return &CertCache{
		iss:    iss,
		lru:    lru,
		redis:  redisClient,
		ttl:    ttl,
		logger: logger,
	}
}

// GetCert retrieves a certificate for the hostname from the TLS ClientHelloInfo,
// checking LRU first, then Redis, then signing a new one.
func (c *CertCache) GetCert(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	hostname := hello.ServerName
	if hostname == "" {
		return nil, fmt.Errorf("cert: empty SNI in ClientHelloInfo")
	}
	return c.GetCertByHostname(hostname)
}

// GetCertByHostname retrieves a certificate for the given hostname string,
// checking LRU first, then Redis, then signing a new one.
func (c *CertCache) GetCertByHostname(hostname string) (*tls.Certificate, error) {
	// Layer 1: LRU cache
	if cert := c.lru.Get(hostname); cert != nil {
		if metrics.CertCacheHits != nil {
			metrics.CertCacheHits.With("lru").Inc()
		}
		return cert, nil
	}

	// Layer 2: Redis cache
	if c.redis != nil {
		cert, err := c.getFromRedis(hostname)
		if err == nil && cert != nil {
			if metrics.CertCacheHits != nil {
				metrics.CertCacheHits.With("redis").Inc()
			}
			c.lru.Put(hostname, cert, c.ttl)
			return cert, nil
		}
		switch {
		case errors.Is(err, errCertKeyMaterial):
			// Redis answered; the entry simply is not ours. A fresh leaf is minted
			// below, so the request is unaffected — but the gauge must NOT be
			// zeroed, because Redis is fine and an availability alert here would
			// send an operator to the wrong system on a routine key rotation.
			c.logger.Warn("cached cert is not decryptable by this process; minting a fresh leaf",
				slog.String("hostname", hostname),
				slog.String("error", err.Error()),
				slog.String("remedy", "entries are scoped to the CA fingerprint, so a CA rotation alone cannot cause this. "+
					"The key that decrypts a cached leaf is derived from the CA private key (local signing) or from the "+
					"cert-cache DEK in Redis at nexus:proxy:cert-cache-dek (remote signing) — NOT from "+
					"CREDENTIAL_ENCRYPTION_KEY, which is a different subsystem. Check that every proxy sharing this "+
					"Redis derives the same key: two nodes holding different cert-cache DEKs under the same CA compute "+
					"the same cache key and will overwrite each other's entries"),
			)
			if metrics.RedisAvailable != nil {
				metrics.RedisAvailable.With().Set(1)
			}
		case err != nil:
			c.logger.Warn("redis get failed, proceeding without cache",
				slog.String("hostname", hostname),
				slog.String("error", err.Error()),
			)
			if metrics.RedisAvailable != nil {
				metrics.RedisAvailable.With().Set(0)
			}
		case metrics.RedisAvailable != nil:
			// Cache miss (cert == nil, err == nil): Redis is reachable but
			// the key is absent. Reset the gauge so stale error state (0)
			// from a prior failed request does not persist indefinitely.
			metrics.RedisAvailable.With().Set(1)
		}
	}

	// Layer 3: Sign new certificate
	if metrics.CertCacheMisses != nil {
		metrics.CertCacheMisses.With().Inc()
	}
	cert, err := c.iss.SignCert(hostname)
	if err != nil {
		return nil, fmt.Errorf("cert: sign for %s: %w", hostname, err)
	}

	// Store in LRU
	c.lru.Put(hostname, cert, c.ttl)

	// Store in Redis (best-effort)
	if c.redis != nil {
		if err := c.putToRedis(hostname, cert); err != nil {
			c.logger.Warn("redis set failed",
				slog.String("hostname", hostname),
				slog.String("error", err.Error()),
			)
			if metrics.RedisAvailable != nil {
				metrics.RedisAvailable.With().Set(0)
			}
		} else if metrics.RedisAvailable != nil {
			metrics.RedisAvailable.With().Set(1)
		}
	}

	return cert, nil
}

// errCertKeyMaterial marks a cached entry that Redis served correctly but this
// process cannot decrypt. It is deliberately distinct from a transport failure:
// Redis answered, so nothing is wrong with Redis, and reporting it as an
// availability problem sends an operator to the wrong system.
var errCertKeyMaterial = errors.New("cert cache: entry is not decryptable by this process")

// redisKey scopes every entry to the CA that minted it.
//
// Without the scope, a CA rotation leaves entries keyed only by hostname holding
// leaves signed by the previous CA and keys encrypted under the previous DEK.
// Each one costs a Redis round-trip and a decrypt failure before the miss path
// re-mints — measured on a live rotation: one WARN per hostname, then
// self-healed once the new entry overwrote the old. Scoping turns that into an
// ordinary miss, and the orphaned entries expire on their own TTL.
//
// The scope is the first 16 hex chars of the CA's SHA-256 fingerprint: enough to
// separate CAs, short enough to keep the key readable in redis-cli.
func (c *CertCache) redisKey(hostname string) string {
	return redisKeyPrefix + c.caScope() + ":" + hostname
}

// caScope returns the short CA fingerprint used to namespace cache entries.
// An issuer without a parsed CA yields "nocert", which cannot collide with a
// real fingerprint and keeps the key well-formed.
func (c *CertCache) caScope() string {
	if c.iss == nil || c.iss.CACert() == nil {
		return "nocert"
	}
	sum := sha256.Sum256(c.iss.CACert().Raw)
	return hex.EncodeToString(sum[:])[:16]
}

// getFromRedis retrieves and decrypts a certificate from Redis.
func (c *CertCache) getFromRedis(hostname string) (*tls.Certificate, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	key := c.redisKey(hostname)
	data, err := c.redis.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil // cache miss, not an error
	}
	if err != nil {
		return nil, fmt.Errorf("redis GET %s: %w", key, err)
	}

	var entry redisCertEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, fmt.Errorf("redis unmarshal %s: %w", key, err)
	}

	// Decode encrypted key and nonce
	encKey, err := base64.StdEncoding.DecodeString(entry.EncryptedKey)
	if err != nil {
		return nil, fmt.Errorf("redis decode encryptedKey: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(entry.Nonce)
	if err != nil {
		return nil, fmt.Errorf("redis decode nonce: %w", err)
	}

	// Decrypt the private key
	privKey, err := c.iss.DecryptPrivateKey(encKey, nonce)
	if err != nil {
		// Wrapped in errCertKeyMaterial so the caller can tell "Redis is sick"
		// from "this entry is not ours". With CA-scoped keys a decryptable-by-
		// nobody entry should no longer be reachable at all; if one is, the DEK
		// changed under a stable CA, which is a different operator problem and
		// must not be reported as a Redis outage.
		return nil, fmt.Errorf("%w: %s: %w", errCertKeyMaterial, hostname, err)
	}

	// Parse the certificate chain PEM
	var certs [][]byte
	rest := []byte(entry.CertChainPEM)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			certs = append(certs, block.Bytes)
		}
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("redis: no certificates found in PEM for %s", hostname)
	}

	// Parse and set Leaf so callers can access it directly without
	// triggering a lazy re-parse on every TLS handshake.
	leaf, err := x509.ParseCertificate(certs[0])
	if err != nil {
		return nil, fmt.Errorf("redis: parse leaf certificate for %s: %w", hostname, err)
	}

	if metrics.RedisAvailable != nil {
		metrics.RedisAvailable.With().Set(1)
	}
	return &tls.Certificate{
		Certificate: certs,
		PrivateKey:  privKey,
		Leaf:        leaf,
	}, nil
}

// putToRedis encrypts the private key and stores the certificate in Redis.
func (c *CertCache) putToRedis(hostname string, cert *tls.Certificate) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ecKey, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return fmt.Errorf("cert: private key is not ECDSA")
	}

	ciphertext, nonce, err := c.iss.EncryptPrivateKey(ecKey)
	if err != nil {
		return fmt.Errorf("cert: encrypt key for redis: %w", err)
	}

	// Encode cert chain as PEM
	var chainPEM []byte
	for _, der := range cert.Certificate {
		chainPEM = append(chainPEM, pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: der,
		})...)
	}

	entry := redisCertEntry{
		EncryptedKey: base64.StdEncoding.EncodeToString(ciphertext),
		CertChainPEM: string(chainPEM),
		Nonce:        base64.StdEncoding.EncodeToString(nonce),
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("cert: marshal redis entry: %w", err)
	}

	return c.redis.Set(ctx, c.redisKey(hostname), data, c.ttl).Err()
}
