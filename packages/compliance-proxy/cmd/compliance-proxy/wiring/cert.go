package wiring

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/tls/cache"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/tls/issuer"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/tls/kms"
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
		signer, err := issuer.NewCommandSigner(
			caCert.PublicKey,
			cfg.CA.KMS.SignCommand,
			time.Duration(cfg.CA.KMS.TimeoutSec)*time.Second,
		)
		if err != nil {
			return CertResult{}, fmt.Errorf("remote signer: %w", err)
		}
		iss, err = issuer.NewIssuerWithRemoteSigner(cfg.CA.CertPath, signer)
		if err != nil {
			return CertResult{}, fmt.Errorf("cert issuer (remote): %w", err)
		}
		slog.Info("CA loaded (remote signing mode)", "certPath", cfg.CA.CertPath)
	} else {
		var err error
		iss, err = issuer.NewIssuer(cfg.CA.CertPath, cfg.CA.KeyPath, kmsProvider)
		if err != nil {
			return CertResult{}, fmt.Errorf("cert issuer: %w", err)
		}
		slog.Info("CA loaded successfully", "certPath", cfg.CA.CertPath)
	}

	// Two-layer cert cache: LRU + Redis.
	certTTL := 23 * time.Hour // slightly less than 24h cert validity
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
