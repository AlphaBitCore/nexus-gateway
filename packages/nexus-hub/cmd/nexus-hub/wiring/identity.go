package wiring

import (
	"context"
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/agentca"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/enrollment"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jwks"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// IdentityResult holds the identity subsystem handles.
type IdentityResult struct {
	JWKSCache *jwks.Cache
	AgentCA   *agentca.CA
	EnrollSvc *enrollment.Service
}

// InitIdentity wires the JWKS cache, Agent CA, and enrollment service.
// The JWKSCache is started here; the caller must call JWKSCache.Close() on
// shutdown. The AgentCA is stateless after load.
func InitIdentity(
	ctx context.Context,
	cfg *config.HubConfig,
	st *store.Store,
	logger *slog.Logger,
) (IdentityResult, error) {
	var jwksCache *jwks.Cache
	if cfg.AuthServer.JWKSURL != "" {
		jwksCache = jwks.New(cfg.AuthServer.JWKSURL, logger)
		logger.Info("JWKS cache started", "url", cfg.AuthServer.JWKSURL)
	} else {
		logger.Warn("authServer.jwksURL is not set; enrollment JWT verification (enterprise-login mode) will be unavailable")
	}

	var agentCA *agentca.CA
	var err error
	if cfg.AgentCA.CertFile != "" && cfg.AgentCA.KeyFile != "" {
		agentCA, err = agentca.NewFromFiles(cfg.AgentCA.CertFile, cfg.AgentCA.KeyFile, logger)
		if err != nil {
			if jwksCache != nil {
				jwksCache.Close()
			}
			return IdentityResult{}, err
		}
	} else {
		caDir := cfg.AgentCA.Dir
		if caDir == "" {
			caDir = ".agent-ca"
		}
		agentCA, err = agentca.New(caDir, logger)
		if err != nil {
			if jwksCache != nil {
				jwksCache.Close()
			}
			return IdentityResult{}, err
		}
	}

	enrollSvc := enrollment.NewService(st)

	return IdentityResult{
		JWKSCache: jwksCache,
		AgentCA:   agentCA,
		EnrollSvc: enrollSvc,
	}, nil
}
