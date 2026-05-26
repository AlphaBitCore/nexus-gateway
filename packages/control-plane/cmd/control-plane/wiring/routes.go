package wiring

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/crypto"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/handler"
	cachehandler "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/cache/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/aiguard/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/simulator/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/exemptions/handler"
	handleriam "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/sessions/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/rulepacks/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/scim/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/jwt"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/apikeystore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/rstokenauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/spillfactory"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/redis/go-redis/v9"
)

// RoutesDeps groups all inputs needed to mount admin and ancillary routes.
type RoutesDeps struct {
	Cfg               *config.Config
	DB                *store.DB
	HubClient         *hub.Client
	IAMEngine         *iam.Engine
	Vault             *crypto.Vault
	MultiVault        *crypto.MultiVault
	AuditWriter       *audit.Writer
	RevocationService *revocation.Service
	RevocationStore   *revocation.Store
	JWTVerifier       *jwtverifier.Verifier
	RedisClient       redis.UniversalClient
	Logger            *slog.Logger
	Ctx               context.Context
}

// InitRoutes builds the admin handler, mounts all admin/internal/my routes,
// and starts the exemption materializer background goroutine.
// Readiness checks are registered separately via InitReadiness/NewReadinessHandler.
func InitRoutes(e *echo.Echo, d RoutesDeps) (*handler.AdminHandler, error) {
	cfg := d.Cfg

	spillStore, err := spillfactory.New(cfg.Spill, d.Logger)
	if err != nil {
		return nil, fmt.Errorf("spillstore init: %w", err)
	}

	adminHandler := &handler.AdminHandler{
		DB:                               d.DB,
		IAM:                              d.IAMEngine,
		Audit:                            d.AuditWriter,
		Hub:                              d.HubClient,
		Vault:                            d.Vault,
		MultiVault:                       d.MultiVault,
		Logger:                           d.Logger,
		SpillStore:                       spillStore,
		ExcludeInternalOpsFromBilledCost: cfg.CostPolicy.ExcludeInternalOpsFromBilledCost,
		Proxy: handler.ProxyConfig{
			ComplianceProxyRuntimeURL: cfg.BFF.ComplianceProxyRuntimeURL,
			ComplianceProxyAPIToken:   cfg.BFF.ComplianceProxyAPIToken,
			AIGatewayURL:              cfg.BFF.AIGatewayURL,
		},
		Revocation:      d.RevocationService,
		RevocationStore: d.RevocationStore,
		Redis:           d.RedisClient,
		AuthRefreshTTL:  24 * time.Hour,
		HubProxyClient: nexushttp.New(nexushttp.Config{
			Timeout:        time.Duration(cfg.HTTPClients.HubProxy.TimeoutSec) * time.Second,
			Caller:         "cp-hub-proxy",
			PropagateReqID: true,
		}),
		ComplianceProxyClient: nexushttp.New(nexushttp.Config{
			Timeout:        time.Duration(cfg.HTTPClients.ComplianceProxyAdmin.TimeoutSec) * time.Second,
			Caller:         "cp-compliance-proxy-admin",
			PropagateReqID: true,
		}),
	}

	if d.DB != nil {
		aiGuardStore := configstore.NewAIGuardStore(d.DB.Pool)
		var dryRunDispatcher aiguard.DryRunDispatcher
		if cfg.BFF.AIGatewayURL != "" && cfg.Auth.InternalServiceToken != "" {
			dispatchTimeout := time.Duration(cfg.AIGuard.DispatchTimeoutSec) * time.Second
			dryRunDispatcher = &aiguard.HTTPDispatcher{
				BaseURL: cfg.BFF.AIGatewayURL,
				Token:   cfg.Auth.InternalServiceToken,
				HTTPClient: nexushttp.New(nexushttp.Config{
					Timeout:        dispatchTimeout,
					Caller:         "cp-dispatch",
					PropagateReqID: true,
				}),
			}
		}
		adminHandler.AIGuard = aiguard.New(aiguard.Deps{
			Store:      aiGuardStore,
			Hub:        d.HubClient,
			Dispatcher: dryRunDispatcher,
			Audit:      d.AuditWriter,
			Logger:     d.Logger,
		})
		// SemanticCacheStore is backed by the same pool as AIGuardStore.
		// Hub uses InvalidateConfig (Category B) so ai-gateway Things reload
		// their in-process L1 snapshot from configstore on the next request.
		// When d.RedisClient is nil (no Redis in env), Poison stays nil and
		// the POST /cache/semantic-feedback endpoint returns 503 gracefully.
		var semanticPoison cachehandler.PoisonAdder
		if d.RedisClient != nil {
			semanticPoison = cachehandler.NewRedisPoisonAdder(d.RedisClient)
		}
		adminHandler.SemanticCache = cachehandler.NewSemanticCacheHandler(cachehandler.SemanticCacheHandlerDeps{
			Store:        configstore.NewSemanticCacheStore(d.DB.Pool),
			Hub:          d.HubClient,
			Audit:        d.AuditWriter,
			Logger:       d.Logger,
			AIGatewayURL: cfg.BFF.AIGatewayURL,
			Poison: semanticPoison,
		})
		adminHandler.ExtractCache = cachehandler.NewExtractCacheHandler(cachehandler.ExtractCacheHandlerDeps{
			Store:  configstore.NewExtractCacheStore(d.DB.Pool),
			Hub:    d.HubClient,
			Audit:  d.AuditWriter,
			Logger: d.Logger,
		})
		adminHandler.RulePacks = rulepacks.New(rulepack.NewStore(d.DB.Pool), d.AuditWriter, d.HubClient)
		adminHandler.Exemption = exemption.New(exemption.Deps{
			DataLayer: func() exemption.DataLayer {
				if adminHandler.ExemptionStore != nil {
					return adminHandler.ExemptionStore
				}
				return d.DB
			}(),
			Hub:    d.HubClient,
			Audit:  d.AuditWriter,
			Logger: d.Logger,
		})
	}

	// Mount admin routes.
	var apiKeyLookup middleware.AdminAPIKeyLookup
	if d.DB != nil {
		apiKeyLookup = apikeystore.New(d.DB.InternalPool())
	}
	adminGroup := e.Group("/api/admin")
	adminGroup.Use(middleware.AdminAuth(middleware.AdminAuthConfig{
		JWTVerifier:  d.JWTVerifier,
		APIKeyLookup: apiKeyLookup,
		Logger:       d.Logger,
	}))
	adminHandler.RegisterAdminRoutes(adminGroup)

	// AI Gateway simulator — bypasses AdminAuth, enforces VK credential itself.
	e.POST("/api/admin/ai-gateway-simulator/forward",
		aigwsim.New(aigwsim.Deps{Audit: d.AuditWriter, Logger: d.Logger}).AIGatewaySimulatorForward)

	// Internal Hub→CP routes.
	internalGroup := e.Group("/api/internal", rstokenauth.Middleware(cfg.Auth.InternalServiceToken))
	handleriam.New(handleriam.Deps{
		Pool:            d.DB.InternalPool(),
		Hub:             d.HubClient,
		Audit:           d.AuditWriter,
		Logger:          d.Logger,
		IAM:             d.IAMEngine,
		Revocation:      d.RevocationService,
		RevocationStore: d.RevocationStore,
		AuthRefreshTTL:  24 * time.Hour,
	}).RegisterInternalAuthRoutes(internalGroup)

	// Personal self-service routes.
	myGroup := e.Group("/api/my")
	myGroup.Use(middleware.AdminAuth(middleware.AdminAuthConfig{
		JWTVerifier:  d.JWTVerifier,
		APIKeyLookup: apiKeyLookup,
		Logger:       d.Logger,
	}))
	me.New(me.Deps{Pool: d.DB.Pool, Hub: d.HubClient, Audit: d.AuditWriter, Logger: d.Logger}).RegisterMyRoutes(myGroup)

	// SCIM 2.0 provisioning.
	if d.DB != nil {
		scimHandler := scim.New(d.DB.InternalPool(), d.Logger, fmt.Sprintf("http://localhost:%d/scim/v2", cfg.Server.Port))
		scimGroup := e.Group("/scim/v2")
		scimHandler.RegisterSCIMRoutes(scimGroup)
	}

	return adminHandler, nil
}

