package cache

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
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

// redisKeyPrefix is the key prefix for certificate entries in Redis.
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
		if err != nil {
			c.logger.Warn("redis get failed, proceeding without cache",
				slog.String("hostname", hostname),
				slog.String("error", err.Error()),
			)
			if metrics.RedisAvailable != nil {
				metrics.RedisAvailable.With().Set(0)
			}
		} else if metrics.RedisAvailable != nil {
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

// getFromRedis retrieves and decrypts a certificate from Redis.
func (c *CertCache) getFromRedis(hostname string) (*tls.Certificate, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	key := redisKeyPrefix + hostname
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
		return nil, fmt.Errorf("redis decrypt key for %s: %w", hostname, err)
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

	key := redisKeyPrefix + hostname
	return c.redis.Set(ctx, key, data, c.ttl).Err()
}
