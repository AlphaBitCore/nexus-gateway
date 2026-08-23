// route_deps.go — the dependency set MountCoreRoutes is handed.
//
// Split from routes.go so the inventory of what the gateway's HTTP surface
// needs stays readable next to nothing else. The two change for different
// reasons: this grows when a subsystem gains a dependency, routes.go grows
// when an endpoint is added.
package wiring

import (
	"log/slog"
	"net/http"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	cache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	geminicache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/gemini"
	cachelayer "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/layer"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
	credmanager "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/credentials/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/passthrough"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	epMetrics "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/ratelimit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/capability"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/wirerewrite"
)

type RouteDeps struct {
	Config         *config.Config
	CacheLayer     *cachelayer.Layer
	DB             *store.DB
	Rdb            redis.UniversalClient
	VKAuth         *vkauth.Authenticator
	RateLimiter    *ratelimit.Limiter
	CredManager    *credmanager.Manager
	RouterResolver *routing.Resolver
	// CapCache is the router's capability snapshot, carried through so the
	// proxy's explicit-model passthrough can apply the same guards the
	// resolver does. Both read it; only the resolver used to have it.
	CapCache *capability.Cache
	Executor *executor.TargetExecutor
	// Resolver is the shared (providerID, modelID) → CallTarget resolver
	// (the same *provtarget.PgResolver the executor holds). The STT
	// streaming-proxy handler resolves its single upstream target through it.
	Resolver          provtarget.Resolver
	HookConfigCache   *pipeline.HookConfigCache
	GWHookRegistry    *hookcore.HookRegistry
	ProviderReg       *provcore.Registry
	HealthTracker     *store.HealthTracker
	AuditWriter       *audit.Writer
	NormalizeReg      *normcore.Registry
	Metrics           *epMetrics.Recorder
	QuotaEngine       *quota.Engine
	ResponseCache     *cache.Cache
	UpstreamClient    *http.Client
	PayloadCapture    *payloadcapture.Store
	StreamingPolicy   *streampolicy.Store
	FormatBridge      *canonicalbridge.Bridge
	Allowlist         *forwardheader.Resolved
	NormEngine        *wirerewrite.Engine
	GeminiCacheMgrSet *geminicache.ManagerSet
	PassthroughCache  *passthrough.Cache
	Logger            *slog.Logger
	// Semantic holds the L2 semantic cache subsystem (all fields nil-safe → degraded mode when absent).
	Semantic SemanticDeps
}

// MountCoreRoutes registers health, metrics, internal admin, and all /v1/* API
// routes. Returns the fully wrapped handler with middleware applied.
// AI Guard classify routes are mounted separately via MountAIGuardRoutes.
