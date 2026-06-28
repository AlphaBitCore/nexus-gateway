package wiring

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
	cachehandler "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/cache/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/simulator/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/assistant"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/aiguard/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/exemptions/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/patternperf"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/rulepacks/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/jwt"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/scim/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/sessions/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/apikeystore"
	handleriam "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/crypto"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/rstokenauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
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

// InitRoutes builds the admin handler and mounts all admin/internal/my routes.
// Readiness checks are registered separately via InitReadiness/NewReadinessHandler.
func InitRoutes(e *echo.Echo, d RoutesDeps) (*handler.AdminHandler, error) {
	cfg := d.Cfg

	// Strip any inbound AI-initiated channel header at the ingress edge (runs before
	// routing, for every request). The web assistant marks its in-process self-calls
	// via an unforgeable context value, never this header, so a copy arriving from a
	// client is a forgery attempt — dropped here so it can never reach the audit
	// writer or be echoed downstream.
	e.Pre(audit.StripInitiatorHeader)

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
			AIGatewayInternalToken:    cfg.Auth.InternalServiceToken,
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
			Store:                  configstore.NewSemanticCacheStore(d.DB.Pool),
			Hub:                    d.HubClient,
			Audit:                  d.AuditWriter,
			Logger:                 d.Logger,
			AIGatewayURL:           cfg.BFF.AIGatewayURL,
			AIGatewayInternalToken: cfg.Auth.InternalServiceToken,
			Poison:                 semanticPoison,
		})
		adminHandler.ExtractCache = cachehandler.NewExtractCacheHandler(cachehandler.ExtractCacheHandlerDeps{
			Store:  configstore.NewExtractCacheStore(d.DB.Pool),
			Hub:    d.HubClient,
			Audit:  d.AuditWriter,
			Logger: d.Logger,
		})
		adminHandler.RulePacks = rulepacks.New(rulepack.NewStore(d.DB.Pool), d.AuditWriter, d.HubClient)
		if cfg.BFF.AIGatewayURL != "" {
			adminHandler.PatternPerf = patternperf.New(cfg.BFF.AIGatewayURL, cfg.Auth.InternalServiceToken, d.Logger)
		}
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
	// apiKeyRehasher lazy-migrates an admin key to the current HMAC keyring
	// version after it admits under an older one. The same
	// apikeystore.Store satisfies both seams.
	var apiKeyRehasher middleware.AdminAPIKeyRehasher
	if d.DB != nil {
		akStore := apikeystore.New(d.DB.InternalPool())
		apiKeyLookup = akStore
		apiKeyRehasher = akStore
	}
	adminGroup := e.Group("/api/admin")
	adminGroup.Use(middleware.AdminAuth(middleware.AdminAuthConfig{
		JWTVerifier:    d.JWTVerifier,
		APIKeyLookup:   apiKeyLookup,
		APIKeyRehasher: apiKeyRehasher,
		Logger:         d.Logger,
	}))
	adminHandler.RegisterAdminRoutes(adminGroup)

	// Web assistant ("Chat with Nexus") — streaming chat under the admin group.
	// Every assistant route additionally gates on the dedicated assistant IAM
	// resource; each tool self-calls admin APIs under the caller's own IAM. The
	// system VK + model come from env (secret never in yaml); the AI Gateway +
	// self-call base URL come from config.
	assistantModel := os.Getenv("NEXUS_ASSISTANT_MODEL")
	if assistantModel == "" {
		assistantModel = "claude-sonnet-4-6"
	}
	// Optional client-selectable allow-list (comma-separated). Empty (the default) → the
	// picker auto-derives every chat model the system VK can route (assistant.ListModels);
	// set this only to pin a narrower allow-list.
	var assistantModels []string
	for _, m := range strings.Split(os.Getenv("NEXUS_ASSISTANT_MODELS"), ",") {
		if m = strings.TrimSpace(m); m != "" {
			assistantModels = append(assistantModels, m)
		}
	}
	assistantCfg := assistant.Config{
		AIGatewayURL: cfg.BFF.AIGatewayURL,
		CPBaseURL:    fmt.Sprintf("http://localhost:%d", cfg.Server.Port),
		SystemVK:     os.Getenv("NEXUS_ASSISTANT_SYSTEM_VK"),
		Model:        assistantModel,
		Models:       assistantModels,
		Spill:        spillStore,
		// Production posture: tells the agent's system prompt the truth about the
		// environment AND arms the prod second-confirm challenge on write actions.
		// Unset ⇒ the assistant presents (and behaves) as non-prod, so prod
		// deployments MUST set it.
		IsProd: os.Getenv("NEXUS_ASSISTANT_PROD") == "1",
		// Governance posture: set NEXUS_ASSISTANT_DISABLE_BODY_READS=1 to withhold
		// the raw-body read tools (the assistant cannot reach raw traffic bodies).
		DisableBodyReads: os.Getenv("NEXUS_ASSISTANT_DISABLE_BODY_READS") == "1",
		// Wall-clock turn backstop (e.g. "10m"); blank/invalid → built-in default.
		// Keep it below the ingress idle/read timeout so the clean turn_deadline SSE
		// error fires before the proxy severs the stream.
		TurnDeadline: parseDurationOr(os.Getenv("NEXUS_ASSISTANT_TURN_DEADLINE"), 0),
		// Multi-replica session-owner registry (the 421 affinity safety net). Uses
		// the shared Redis client + this instance's hostname as the owner id; nil
		// Redis (single replica / no Redis) disables it.
		Redis:   d.RedisClient,
		OwnerID: assistantOwnerID(),
		// In-process self-call: dispatch the agent's admin tools straight into
		// this CP router (no loopback HTTP, unforgeable AI-initiated audit stamp).
		Dispatcher: e,
		Logger:     d.Logger,
	}
	// The prod posture is a safety knob whose forgotten state is the unsafe
	// state (weaker confirms + the model told it is non-prod), so the boot log
	// states it explicitly either way — the deploy preflight greps for this.
	if assistantCfg.IsProd {
		d.Logger.Info("assistant production posture: ENABLED (NEXUS_ASSISTANT_PROD=1)")
	} else {
		d.Logger.Warn("assistant production posture: disabled (NEXUS_ASSISTANT_PROD unset) — REQUIRED on production deployments")
	}
	// d.DB is nil when CP boots without a database (a supported mode); leave Pool nil
	// so the assistant degrades to in-memory stores rather than nil-dereferencing.
	if d.DB != nil {
		assistantCfg.Pool = d.DB.Pool
	}
	assistant.New(assistantCfg).RegisterAssistantRoutes(adminGroup, d.IAMEngine)

	// AI Gateway simulator — admin debugging proxy. Mounts on adminGroup so the
	// admin-session middleware gates it (matching the handler's own doc); the
	// gateway-side VK stays the upstream credential and no extra IAM action gate
	// is applied. Final path is unchanged: /api/admin/ai-gateway-simulator/forward.
	adminGroup.POST("/ai-gateway-simulator/forward",
		aigwsim.New(aigwsim.Deps{Logger: d.Logger}).AIGatewaySimulatorForward)

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
		JWTVerifier:    d.JWTVerifier,
		APIKeyLookup:   apiKeyLookup,
		APIKeyRehasher: apiKeyRehasher,
		Logger:         d.Logger,
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

// parseDurationOr parses a Go duration string (e.g. "10m"), returning def on a
// blank or malformed value so a typo in the env never wedges startup.
func parseDurationOr(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

// assistantOwnerID identifies this CP instance for the assistant session-owner
// registry. The hostname is the pod identity in a K8s/containerized deploy; an
// empty hostname (unusual) disables the registry (blank OwnerID → nil registry).
func assistantOwnerID() string {
	h, _ := os.Hostname()
	return h
}
