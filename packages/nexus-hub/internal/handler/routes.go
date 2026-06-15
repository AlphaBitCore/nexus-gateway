package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/handler/hubapi"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/agentca"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/enrollment"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/handler/bootstrap"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/handler/enroll"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/scheduler"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jwks"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/handler/diag"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/traffic/ingest/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/traffic/ingest/spill"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/ws"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillupload"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// RouteConfig holds all dependencies for setting up routes.
type RouteConfig struct {
	Echo         *echo.Echo
	Mgr          *manager.Manager
	WSServer     *ws.Server
	Scheduler    *scheduler.Scheduler
	Enrollment   *enrollment.Service
	MQProducer   mq.Producer
	ServiceToken string
	// HubConfigToken gates the two config-AUTHORITY route groups — /api/hub
	// (config-write) and /api/v1/admin/alerts (admin) — separately from
	// ServiceToken. Control Plane is the sole caller of both
	// groups and holds this token; the data-plane services (ai-gateway,
	// compliance-proxy) hold only ServiceToken, so a leak of their service token
	// can no longer write fleet config. ServiceToken still gates
	// /api/internal/things + the WS registration path.
	HubConfigToken string
	Store          *store.Store
	AgentCA        *agentca.CA
	Raiser         *alerting.Raiser
	AlertStore     *alerting.Store
	AlertRules     alerting.RuleRegistry
	AlertSenders   alerting.SenderRegistry
	// CatB is the Cat B loader registry consumed by SingleConfigPull
	// to assemble authoritative hook / policy / domain state from
	// CP-owned business tables. Nil disables the dispatch and every
	// config pull goes through thing_config_template.state (the
	// pre-P0-C code path).
	CatB *store.CatBRegistry

	// SpillStore is the spill backend the Hub *reads* from (Control
	// Plane traffic-detail resolves spill_ref → bytes via this) and
	// the localfs blob upload endpoint *writes* into. The audit
	// ingestion path NEVER calls SpillStore.Put — agents choose
	// inline-vs-spill themselves and ship a SpillRef in the audit
	// envelope. nil = no spill support (every agent body must be inline).
	SpillStore        spillstore.SpillStore
	SpillBackend      string // "localfs" | "s3"
	SpillPerObjectCap int64

	// SpillSecrets + SpillDedup gate the pre-signed upload-URL flow.
	// SpillSecrets serves Active() at mint time and Lookup() at verify
	// time; SpillDedup is the Redis SETNX one-shot consumer. Either being
	// nil disables the corresponding endpoint with 503 at request time.
	SpillSecrets *spillupload.SecretStore
	SpillDedup   spillupload.Dedup

	// OpsDiagPool is the pgx pool the diag-events:batch handler writes
	// crash drains into. When nil, the route is not registered (e.g.
	// test harnesses that don't exercise the drain path).
	OpsDiagPool *pgxpool.Pool

	// OpsLogger is the slog handle used by the diag drain handler. If
	// nil and OpsDiagPool is set, the handler falls back to slog.Default.
	OpsLogger *slog.Logger

	// JWKSCache verifies enrollment JWTs when enterprise-login mode is active.
	// Nil when cfg.AuthServer.JWKSURL is not set; Bearer enrollment then returns 503.
	JWKSCache *jwks.Cache

	// CpIssuer is pinned via jwt.WithIssuer during enrollment JWT
	// verification. Must equal cfg.AuthServer.Issuer on the CP side.
	// Required alongside JWKSCache; empty disables issuer pinning
	// (useful only for unit tests).
	CpIssuer string

	// CpURL is the Control Plane base URL surfaced via the agent
	// bootstrap endpoint so agents do not need to embed it in their
	// per-device YAML. Empty disables the bootstrap endpoint.
	CpURL string

	// DBPool is reused by the agent bootstrap endpoint to read the
	// current device_auth_mode from system_metadata. Optional — when
	// nil the bootstrap endpoint returns the safe default
	// "mtls-only".
	DBPool *pgxpool.Pool

	// HubID + HubLocalURL drive the runtime introspection bridge
	// (GET /api/hub/things/:id/runtime). HubID enables short-circuiting
	// for self-calls; HubLocalURL is the URL the bridge uses to reach
	// hub's own /debug/runtime when the target id matches HubID.
	HubID       string
	HubLocalURL string

	// NormalizeAgentAudit, when non-nil, is the shared/normalize closure
	// the agent-audit handler uses to project captured bytes into the
	// canonical NormalizedPayload shape. Wired from cmd/nexus-hub via
	// normalize.BuildAuditFn.
	NormalizeAgentAudit func(direction, contentType, adapterType, model, path string, stream bool, body []byte) (raw json.RawMessage, status, errReason string)
}

// Hub exposes no user-JWT-protected HTTP surface. User/admin operations are
// proxied through Control Plane's /api/admin/* routes (which carry the
// auth-server JWT) and reach Hub via /api/hub/* using the internal service
// token. Direct browser or end-user traffic to Hub is not part of the
// deployment topology. If that changes, mount jwtverifier.Middleware on the
// new route group here and wire a jwtverifier.Verifier in cmd/nexus-hub/main.go.

// SetupRoutes registers all Hub HTTP routes. Returns the constructed
// enroll.EnrollmentAPI (when AgentCA is configured) so the caller can invoke
// Close() at shutdown to stop background goroutines. Callers that do
// not need teardown can ignore the return value.
func SetupRoutes(cfg RouteConfig) *enroll.EnrollmentAPI {
	hubAPI := &hubapi.HubAPI{
		Mgr:         cfg.Mgr,
		Scheduler:   cfg.Scheduler,
		Enrollment:  cfg.Enrollment,
		DLQPool:     cfg.DBPool,
		DLQProducer: cfg.MQProducer,
	}

	// /api/hub is the CP→Hub config-WRITE surface (shadow /
	// desired-state push). It is gated by the dedicated HubConfigToken, NOT the
	// fleet-wide ServiceToken, so a compromised data-plane service (which holds
	// only ServiceToken) cannot inject fleet config. CP is the sole caller.
	hub := cfg.Echo.Group("/api/hub", ServiceAuth(cfg.HubConfigToken))
	hub.POST("/config/update", hubAPI.ConfigUpdate)
	hub.GET("/things", hubAPI.ListThings)
	// Static prefix BEFORE the parametric :id route — Echo matches in
	// registration order, so `/things/overrides` would otherwise be
	// captured by `/things/:id` and never reach ListGlobalOverrides.
	hub.GET("/things/overrides", hubAPI.ListGlobalOverrides)
	hub.GET("/things/:id", hubAPI.GetThing)
	hub.GET("/things/:id/shadow", hubAPI.GetThingShadow)
	hub.GET("/things/:id/service-meta", hubAPI.GetThingServiceMeta)
	hub.POST("/things/:id/resync", hubAPI.ResyncThing)
	hub.GET("/things/:id/overrides", hubAPI.ListThingOverrides)
	hub.PUT("/things/:id/overrides/:configKey", hubAPI.SetThingOverride)
	hub.DELETE("/things/:id/overrides/:configKey", hubAPI.ClearThingOverride)
	hub.GET("/drift", hubAPI.ListDrift)
	hub.GET("/config/history", hubAPI.ListConfigHistory)
	hub.GET("/config/catalog", hubAPI.ListConfigCatalog)
	hub.GET("/jobs", hubAPI.ListJobs)
	hub.GET("/jobs/:id", hubAPI.GetJob)
	hub.GET("/jobs/:id/runs", hubAPI.ListJobRuns)
	hub.PUT("/jobs/:id", hubAPI.UpdateJob)
	hub.POST("/jobs/:id/trigger", hubAPI.TriggerJob)
	// Dead-letter queue admin endpoints. List paginates over traffic_event_dlq
	// rows (newest first); retry republishes a single row to its original MQ
	// subject and deletes the row on publish success. Static prefix BEFORE
	// the parametric :id route per Echo's match-in-registration-order rule.
	hub.GET("/dlq", hubAPI.ListDLQ)
	hub.POST("/dlq/:id/retry", hubAPI.RetryDLQ)
	hub.POST("/enrollment/token", hubAPI.GenerateEnrollmentToken)
	hub.GET("/enrollment/tokens", hubAPI.ListEnrollmentTokens)

	// Runtime introspection bridge (/api/hub/things/:id/runtime).
	if cfg.Store != nil {
		bridge := &diag.RuntimeBridgeAPI{
			ServiceToken: cfg.ServiceToken,
			HubID:        cfg.HubID,
			HubLocalURL:  cfg.HubLocalURL,
		}
		// Assign through a concrete-nil guard so the store.PgxPool
		// interface field stays nil when Pool() returns nil (test setups
		// via NewWithPgxPool) — otherwise the typed-nil pointer wraps
		// into a non-nil interface and downstream nil checks misfire.
		if p := cfg.Store.Pool(); p != nil {
			bridge.DB = p
		}
		hub.GET("/things/:id/runtime", bridge.Runtime)
	}

	thingsAPI := &hubapi.InternalThingsAPI{
		Mgr:        cfg.Mgr,
		MQProducer: cfg.MQProducer,
		CatB:       cfg.CatB,
	}

	deviceAuth := enroll.DeviceOrServiceAuth(cfg.Store, cfg.ServiceToken)
	things := cfg.Echo.Group("/api/internal/things", deviceAuth)
	things.POST("/register", thingsAPI.Register)
	things.POST("/heartbeat", thingsAPI.Heartbeat)
	things.POST("/shadow", thingsAPI.ShadowReport)
	// Dedicated break-glass HTTP fallback (matches the thingclient HTTP path
	// thingclient.SendBreakGlassShadowReport posts to). The route itself is the
	// break-glass signal; the handler stamps Reason="break_glass" and enforces
	// the server-side allowlist + schema gate before dispatch.
	things.POST("/shadow/break-glass", thingsAPI.BreakGlassReport)
	things.GET("/config", thingsAPI.BulkConfigPull)
	things.GET("/config/:key", thingsAPI.SingleConfigPull)
	things.POST("/audit", thingsAPI.AuditUpload)
	things.POST("/deregister", thingsAPI.Deregister)
	things.POST("/exemption", thingsAPI.ExemptionUpload)
	things.GET("/update-check", thingsAPI.UpdateCheck)
	// Per-agent attestation public key lookup, called by CP's
	// AttestationKeyCache loader. Same deviceAuth group so service-token
	// (CP) and device-token (agent self-introspection) callers both work.
	things.GET("/:id/attestation-pubkey", thingsAPI.GetAttestationPubKey)

	agentAuditAPI := &audit.AgentAuditAPI{
		MQProducer: cfg.MQProducer,
		Normalize:  cfg.NormalizeAgentAudit,
	}
	things.POST("/agent-audit", agentAuditAPI.UploadAgentAudit)

	// Spill upload mint + blob endpoints. The mint endpoint sits under
	// /api/internal/things/* so enroll.DeviceOrServiceAuth (mTLS thing
	// identity) gates URL issuance. The blob endpoint sits outside the
	// deviceAuth group because the HMAC token is the authorisation
	// primitive — agents on networks that strip client certs at the LB
	// still need to complete uploads.
	spillUploadAPI := &spill.SpillUploadAPI{
		Spill:        cfg.SpillStore,
		SpillBackend: cfg.SpillBackend,
		PerObjectCap: cfg.SpillPerObjectCap,
		Secrets:      cfg.SpillSecrets,
		Dedup:        cfg.SpillDedup,
		Logger:       cfg.OpsLogger,
	}
	things.POST("/spill-uploads", spillUploadAPI.MintSpillUpload)
	if cfg.SpillBackend == "localfs" {
		cfg.Echo.PUT("/api/internal/spill/blob/:token", spillUploadAPI.PutSpillBlob)
	}

	// Crash-buffer drain — the agent posts pending diag_event rows from
	// its local SQLCipher buffer on startup. Mounted under the same
	// /api/internal/things/* group so enroll.DeviceOrServiceAuth covers it.
	// Echo treats `:batch` as a literal segment (no leading colon ⇒ not
	// a path parameter), matching the spec route shape.
	if cfg.OpsDiagPool != nil {
		// Concrete-nil already screened above; assign through the
		// interface field directly. Same typed-nil-vs-interface caveat
		// applies as diag.RuntimeBridgeAPI, but the cfg.OpsDiagPool != nil
		// guard already weeds out the nil case.
		diagAPI := &diag.DiagDrainAPI{Pool: cfg.OpsDiagPool, Logger: cfg.OpsLogger}
		things.POST("/diag-events:batch", diagAPI.UploadDiagEvents)
	}

	// Internal alerting API (/api/v1/alerts/*): data-plane producers
	// (compliance-proxy, ai-gateway) use their device token; Hub-internal
	// callers use the service token. Gated on cfg.Raiser so test harnesses
	// that skip the full alerting stack can still call SetupRoutes.
	if cfg.Raiser != nil {
		alerts := cfg.Echo.Group("/api/v1/alerts", deviceAuth)
		alerts.POST("/raise", alertCallerScoped(alerting.HandleRaise(cfg.Raiser)))
		alerts.POST("/resolve", alertCallerScoped(alerting.HandleResolve(cfg.Raiser)))
	}

	// Admin alerting API (/api/v1/admin/alerts/*): service-token gated;
	// Control Plane proxies admin UI calls here. Static sub-paths (/rules,
	// /channels) MUST be registered before the parametric /:id route —
	// Echo matches in registration order.
	if cfg.AlertStore != nil && cfg.AlertRules != nil && cfg.AlertSenders != nil {
		adminH := &alerting.AdminHandlers{
			Store:   cfg.AlertStore,
			Rules:   cfg.AlertRules,
			Senders: cfg.AlertSenders,
		}
		// admin-alerts is a CP-proxied admin surface, gated by
		// the dedicated HubConfigToken (same authority class as /api/hub), NOT the
		// fleet-wide ServiceToken. CP is the sole caller.
		admin := cfg.Echo.Group("/api/v1/admin/alerts", ServiceAuth(cfg.HubConfigToken))
		// Rule routes first (static prefix).
		admin.GET("/rules", adminH.ListRules)
		admin.GET("/rules/:id", adminH.GetRule)
		admin.PUT("/rules/:id", adminH.UpdateRule)
		admin.POST("/rules/:id/reset", adminH.ResetRule)
		// Channel routes next (static prefix).
		admin.GET("/channels", adminH.ListChannels)
		admin.POST("/channels", adminH.CreateChannel)
		admin.GET("/channels/:id", adminH.GetChannel)
		admin.PUT("/channels/:id", adminH.UpdateChannel)
		admin.DELETE("/channels/:id", adminH.DeleteChannel)
		admin.POST("/channels/:id/test", adminH.ChannelTest)
		// Alert routes last (parametric /:id would shadow static siblings).
		admin.GET("", adminH.ListAlerts)
		admin.GET("/:id", adminH.GetAlert)
		admin.POST("/:id/ack", adminH.AckAlert)
		admin.POST("/:id/resolve", adminH.ResolveAlert)
	}

	// Agent bootstrap (public; unauthenticated): pre-enrollment agents
	// fetch the Control Plane URL + current device_auth_mode from this
	// endpoint instead of hard-coding the CP URL in their YAML.
	if cfg.CpURL != "" {
		bootstrap := &bootstrap.AgentBootstrapHandler{CpURL: cfg.CpURL}
		// Same typed-nil interface guard as the runtime bridge: only
		// stamp the DB seam when the concrete pool pointer is non-nil so
		// `if h.DB != nil` in body() still short-circuits cleanly under
		// test wirings that pass DBPool=nil deliberately.
		if cfg.DBPool != nil {
			bootstrap.DB = cfg.DBPool
		}
		cfg.Echo.GET("/api/public/agent-bootstrap", bootstrap.Handle)
	}

	var enrollAPI *enroll.EnrollmentAPI
	if cfg.AgentCA != nil {
		enrollAPI = &enroll.EnrollmentAPI{
			Mgr:        cfg.Mgr,
			Enrollment: cfg.Enrollment,
			CA:         cfg.AgentCA,
			JWKSCache:  cfg.JWKSCache,
			CpIssuer:   cfg.CpIssuer,
			Logger:     cfg.OpsLogger,
			// Back the enrollment-JWT replay guard with the shared
			// Redis SETNX dedup so a captured JWT cannot be replayed across a Hub
			// restart (or on another Hub). nil (no Redis) keeps the in-memory-only
			// single-Hub guard.
			JTIDedup: cfg.SpillDedup,
		}
		enrollAPI.Init()
		cfg.Echo.POST("/api/internal/things/enroll", enrollAPI.Enroll)
		// Device-token rotation. Sits inside the deviceAuth group so the
		// caller's current (still-valid) device token authenticates the
		// rotation; the resolved Thing in context is the identity whose
		// token is rotated.
		things.POST("/renew-token", enrollAPI.RenewToken)
	}

	if cfg.WSServer != nil {
		cfg.Echo.GET("/ws", echo.WrapHandler(http.HandlerFunc(cfg.WSServer.HandleUpgrade)))
	}

	return enrollAPI
}

// alertCallerScoped adapts a raw alerting http.Handler to an echo.HandlerFunc
// that injects the authenticated caller identity into the request context so
// the raise/resolve handlers can enforce per-Thing scoping.
//
// The DeviceOrServiceAuth middleware sets the resolved Thing on the Echo
// context for device-token callers and leaves it nil for service-token callers
// (CP / Hub-internal). We translate that into an alerting.Caller: a nil Thing
// means the service token authenticated the call (unrestricted), a non-nil
// Thing means a device token (restricted to its own target on raise, no
// resolve at all).
func alertCallerScoped(h http.Handler) echo.HandlerFunc {
	return func(c echo.Context) error {
		caller := alerting.Caller{IsService: true}
		if thing := enroll.ThingFromContext(c); thing != nil {
			caller = alerting.Caller{IsService: false, ThingID: thing.ID}
		}
		req := c.Request()
		req = req.WithContext(alerting.WithCaller(req.Context(), caller))
		h.ServeHTTP(c.Response(), req)
		return nil
	}
}
