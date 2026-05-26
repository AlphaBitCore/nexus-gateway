package config

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/conn"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/killswitch"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/identity"
)

// ConnManagerReader abstracts reading from conn.Manager for dependency injection.
type ConnManagerReader interface {
	ActiveCount() int64
	ActiveConnections() []conn.ConnInfo
}

// BreakGlassReporter is the delivery hook for break-glass shadow reports.
// Satisfied by *thingclient.Client. The handler treats a non-nil error as
// "Hub delivery failed, spool to pending buffer" — the local apply has
// already happened before this call.
type BreakGlassReporter interface {
	SendBreakGlassShadowReport(
		ctx context.Context,
		key string,
		state json.RawMessage,
		keyVer int64,
		reason, sourceIP, actorTokenID string,
	) error
}

// BreakGlassVersionSource supplies the Hub-known per-Thing version counters
// that drive the newVer calculation per spec §5.2 step 3:
//
//	newVer = max(DesiredVer, ReportedVer) + 1
//
// Using Hub-aware versions (instead of a proxy-local counter) is required: on
// the Hub side, handleBreakGlassReport applies the template only when
// `msg.ReportedVer > currentTemplate.Version`. A local counter starting at 1
// would be dropped whenever Hub already knows a newer version.
//
// Satisfied by *thingclient.Client.
type BreakGlassVersionSource interface {
	DesiredVer() int64
	ReportedVer() int64
}

// ExemptionRebuilder is the interface satisfied by exemption.Store.Rebuild.
type ExemptionRebuilder interface {
	Rebuild([]identity.ActiveExemption)
}

// RuntimeDeps holds dependencies injected into the runtime API handlers.
//
// The shape is intentionally shadow-driven: every mutating surface is now
// served by the break-glass PUT /runtime/config/{key} handler, which routes
// through the per-key snapshotter and the apply helpers in shadow_apply.go.
// The in-memory stores (ExemptionStore) are owned by main and applied via
// OnConfigChanged — they do not need to live on RuntimeDeps because no
// handler calls Create/Update/Delete/Set on them directly.
type RuntimeDeps struct {
	KillSwitch     *killswitch.KillSwitch
	ConnManager    ConnManagerReader
	StartTime      time.Time   // process start time (for uptime calculation)
	RedisChecker   func() bool // returns true if Redis is connected
	Logger         *slog.Logger
	Readiness      *atomic.Bool     // controls 200 vs 503 on /healthz
	ExemptionStore *exemption.Store // temporary compliance-hook exemptions (still wired so future read tooling can inspect the store even when shadow apply is the only writer)

	// --- /runtime/* surface wiring (runtimeapi-slimming) ---

	// ThingID and ThingType identify this proxy instance in /runtime/config
	// and /runtime/health response bodies.
	ThingID   string
	ThingType string

	// Thingclient exposes the sync-version accessors for the shadow client
	// that backs this proxy. Satisfied by *thingclient.Client in production.
	Thingclient ThingclientSnapshotter

	// Snapshotters provide the per-config_key read-only views consumed by
	// HandleRuntimeConfig / HandleRuntimeConfigKey. Each is satisfied by the
	// corresponding internal store (exemption.Store, KillSwitch).
	ExemptionSnap  ExemptionSnapshotter
	KillswitchSnap KillswitchSnapshotter

	// Rebuilders are invoked by the break-glass PUT handler after durable
	// event-log + version bump. Each is satisfied by the same concrete store
	// whose snapshot is served on GET.
	ExemptionRebuilder ExemptionRebuilder

	// Health supplies per-subsystem liveness probes for /runtime/health.
	Health HealthChecks

	// DataDir is the proxy data directory. Break-glass writes its durable
	// event log and pending-report buffer under DataDir. Empty means
	// break-glass is disabled — PUT /runtime/config/{key} returns 503.
	DataDir string

	// BreakGlassReporter is the shadow-report hook for break-glass writes.
	// Satisfied by *thingclient.Client in production. When nil, break-glass
	// still applies and event-logs locally; the pending buffer on disk is
	// retried on the next successful connection.
	BreakGlassReporter BreakGlassReporter
}

// HealthzResponse is the JSON body returned by GET /healthz.
type HealthzResponse struct {
	Status            string  `json:"status"`
	UptimeSeconds     float64 `json:"uptimeSeconds"`
	ConnectionsActive int64   `json:"connectionsActive"`
	BumpEnabled       bool    `json:"bumpEnabled"`
	RedisConnected    bool    `json:"redisConnected"`
}

// ConnectionsResponse is the JSON body returned by GET /connections.
type ConnectionsResponse struct {
	Connections []conn.ConnInfo `json:"connections"`
	Total       int             `json:"total"`
}

// WriteJSON encodes v as JSON and writes it to w with the given status code.
func WriteJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
