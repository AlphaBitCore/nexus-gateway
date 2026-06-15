package wiring

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/tls/cache"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/tls/issuer"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/kms"
)

// CertResult holds the components created by InitCertIssuer.
type CertResult struct {
	Issuer    *issuer.Issuer
	CertCache *cache.CertCache
}

// InitCertIssuer initializes the CA certificate issuer (with optional KMS
// unwrap or remote signing) and the two-layer cert cache (LRU + Redis).
// It also performs cert pre-warming for configured domain allowlists.
func InitCertIssuer(cfg *config.Config, redisClient redis.UniversalClient, logger *slog.Logger) (CertResult, error) {
	var kmsProvider kms.KMSProvider
	if cfg.CA.KMS.Provider != "" && cfg.CA.KMS.Provider != "noop" {
		switch cfg.CA.KMS.Provider {
		case "command":
			cmdProvider, err := kms.NewCommandProvider(
				cfg.CA.KMS.Command,
				time.Duration(cfg.CA.KMS.TimeoutSec)*time.Second,
			)
			if err != nil {
				return CertResult{}, fmt.Errorf("KMS command provider: %w", err)
			}
			kmsProvider = cmdProvider
		default:
			return CertResult{}, fmt.Errorf("unknown KMS provider: %s", cfg.CA.KMS.Provider)
		}
		slog.Info("CA private key will be unwrapped via KMS", "provider", kmsProvider.Name())
	}

	// Remote signing mode — the CA key never leaves KMS.
	var iss *issuer.Issuer
	if cfg.CA.KMS.SigningMode == "remote" {
		caCertPEM, err := os.ReadFile(cfg.CA.CertPath)
		if err != nil {
			return CertResult{}, fmt.Errorf("read CA cert for remote signer: %w", err)
		}
		block, _ := pem.Decode(caCertPEM)
		if block == nil {
			return CertResult{}, fmt.Errorf("no CERTIFICATE PEM block in CA cert file")
		}
		caCert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return CertResult{}, fmt.Errorf("parse CA cert: %w", err)
		}
		kmsTimeout := time.Duration(cfg.CA.KMS.TimeoutSec) * time.Second
		signer, err := issuer.NewCommandSigner(
			caCert.PublicKey,
			cfg.CA.KMS.SignCommand,
			kmsTimeout,
		)
		if err != nil {
			return CertResult{}, fmt.Errorf("remote signer: %w", err)
		}

		// Self-bootstrapping KMS envelope encryption for the cert cache:
		// the AES key is derived from a KMS-managed DEK (wrapped blob in
		// Redis), never from public CA bytes. Fail-closed on any missing
		// dependency — the issuer must NOT fall back to a CA-derived key.
		if redisClient == nil {
			return CertResult{}, fmt.Errorf("remote signing mode requires Redis for the cert-cache DEK envelope (set redis.addrs / REDIS_ADDRS)")
		}
		if len(cfg.CA.KMS.EncryptCommand) == 0 {
			return CertResult{}, fmt.Errorf("remote signing mode requires ca.kms.encryptCommand (KMS encrypt argv; grant kms:Encrypt)")
		}
		if len(cfg.CA.KMS.Command) == 0 {
			return CertResult{}, fmt.Errorf("remote signing mode requires ca.kms.command (KMS decrypt argv; grant kms:Decrypt)")
		}
		dekEncryptor, err := kms.NewCommandEncryptor(cfg.CA.KMS.EncryptCommand, kmsTimeout)
		if err != nil {
			return CertResult{}, fmt.Errorf("cert-cache DEK encryptor: %w", err)
		}
		dekDecryptor, err := kms.NewCommandProvider(cfg.CA.KMS.Command, kmsTimeout)
		if err != nil {
			return CertResult{}, fmt.Errorf("cert-cache DEK decryptor: %w", err)
		}
		bootCtx, bootCancel := context.WithTimeout(context.Background(), 2*kmsTimeoutOrDefault(kmsTimeout)+10*time.Second)
		iss, err = issuer.NewIssuerWithRemoteSigner(
			bootCtx, cfg.CA.CertPath, signer,
			newRedisDEKStore(redisClient), dekEncryptor, dekDecryptor,
		)
		bootCancel()
		if err != nil {
			return CertResult{}, fmt.Errorf("cert issuer (remote): %w", err)
		}
		slog.Info("CA loaded (remote signing mode); cert-cache DEK bootstrapped via KMS envelope", "certPath", cfg.CA.CertPath)
	} else {
		var err error
		iss, err = issuer.NewIssuer(cfg.CA.CertPath, cfg.CA.KeyPath, kmsProvider)
		if err != nil {
			return CertResult{}, fmt.Errorf("cert issuer: %w", err)
		}
		slog.Info("CA loaded successfully", "certPath", cfg.CA.CertPath)
	}

	// Two-layer cert cache: LRU + Redis. The TTL is DERIVED from the leaf
	// validity (not an independent magic number) so the cache can never be
	// configured to outlive the cert it stores and serve an expired cert.
	// The LRU additionally clamps every lease to the leaf's NotAfter as a
	// per-entry backstop (see tls/cache.leaseExpiry).
	certTTL := issuer.LeafValidity - time.Hour // one hour below leaf validity
	lruCache := cache.NewLRUCache(256)
	certCache := cache.NewCertCache(iss, lruCache, redisClient, certTTL, logger)

	// Cert pre-warming for configured domain allowlists.
	domains := ExtractDomains(cfg.AccessControl.DomainAllowlist)
	if len(domains) > 0 {
		warmCtx, warmCancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := cache.Warmup(warmCtx, certCache, domains, logger); err != nil {
			slog.Warn("cert pre-warming had errors", "error", err)
		}
		warmCancel()
	}

	return CertResult{Issuer: iss, CertCache: certCache}, nil
}

// kmsTimeoutOrDefault mirrors the kms package's per-command default (10s when
// unset) so the bootstrap context comfortably outlives the (possibly two)
// KMS sub-commands the DEK bootstrap may run.
func kmsTimeoutOrDefault(d time.Duration) time.Duration {
	if d <= 0 {
		return 10 * time.Second
	}
	return d
}

// redisDEKStore adapts a go-redis UniversalClient to
// issuer.CertCacheDEKStore, translating the chainable API + redis.Nil
// sentinel into the (blob, found, err) / (won, err) contract the cert-cache
// DEK bootstrap expects.
type redisDEKStore struct {
	rdb redis.UniversalClient
}

func newRedisDEKStore(rdb redis.UniversalClient) *redisDEKStore {
	return &redisDEKStore{rdb: rdb}
}

func (s *redisDEKStore) GetWrappedDEK(ctx context.Context) ([]byte, bool, error) {
	blob, err := s.rdb.Get(ctx, issuer.CertCacheDEKRedisKey).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return blob, true, nil
}

func (s *redisDEKStore) SetWrappedDEKIfAbsent(ctx context.Context, blob []byte) (bool, error) {
	// TTL 0 = no expiry: the wrapped DEK is the long-lived KEK for the whole
	// deployment. Rotation is an explicit DEL of issuer.CertCacheDEKRedisKey.
	return s.rdb.SetNX(ctx, issuer.CertCacheDEKRedisKey, blob, 0).Result()
}
