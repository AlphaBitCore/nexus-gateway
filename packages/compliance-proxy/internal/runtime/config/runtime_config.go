package config

import (
	"context"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/identity"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/interception"
	cphttperr "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/httperr"
)

// ExemptionSnapshotter exposes a read-only view of the exemptions
// shadow state. Satisfied by *exemption.Store.
type ExemptionSnapshotter interface {
	Snapshot() identity.ActiveExemptions
}

// KillswitchSnapshotter exposes the killswitch shadow surface. Snapshot
// powers GET /runtime/config/killswitch; ApplyBreakGlass is invoked by the
// PUT /runtime/config/killswitch break-glass handler after the durable
// event log entry. Satisfied by *KillSwitch.
type KillswitchSnapshotter interface {
	Snapshot() interception.Killswitch
	ApplyBreakGlass(ks interception.Killswitch) error
}

// ThingclientSnapshotter exposes the shadow-sync version accessors used by
// /runtime/sync-status and the per-key version block in /runtime/config.
// Satisfied by *thingclient.Client.
type ThingclientSnapshotter interface {
	DesiredVer() int64
	ReportedVer() int64
	// KeyVersion returns the per-config_key version reported by Hub, or 0
	// when the key has not been observed yet.
	KeyVersion(key string) int64
	// LastReportedAt is the RFC3339 timestamp of the last successful
	// shadow_report, or empty when no report has been sent.
	LastReportedAt() string
}

// HealthChecks is the injection point for /runtime/health. Production wires
// it to probes for Hub WS, upstream TLS, audit spool, and DB. Leaving Run
// nil causes HandleRuntimeHealth to report an empty check map with overall
// status "ok" — callers should supply at least one probe.
type HealthChecks struct {
	Run func(ctx context.Context) map[string]string
}

// KnownRuntimeConfigKeys is the whitelist used by /runtime/config and
// /runtime/config/{key}. Response order is not a wire contract — the JSON
// encoder sorts object keys alphabetically.
var KnownRuntimeConfigKeys = []string{
	"killswitch",
	"exemptions",
}

// HandleRuntimeConfig returns a composite snapshot of every runtime
// config_key plus the thing identity and shadow-sync cursor.
func HandleRuntimeConfig(deps RuntimeDeps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			cphttperr.WriteError(w, http.StatusMethodNotAllowed, "method not allowed", "method_not_allowed", "METHOD_NOT_ALLOWED")
			return
		}

		configs := make(map[string]map[string]any, len(KnownRuntimeConfigKeys))
		for _, k := range KnownRuntimeConfigKeys {
			configs[k] = map[string]any{
				"version": keyVersion(deps, k),
				"state":   snapshotFor(deps, k),
			}
		}

		WriteJSON(w, http.StatusOK, map[string]any{
			"thingId":     deps.ThingID,
			"thingType":   deps.ThingType,
			"desiredVer":  desiredVer(deps),
			"reportedVer": reportedVer(deps),
			"inSync":      inSync(deps),
			"reportedAt":  lastReportedAt(deps),
			"configs":     configs,
		})
	})
}

// HandleRuntimeConfigKey returns the snapshot for a single shadow-managed
// config_key. The key is bound at registration time (one handler per route)
// so the handler does not parse the URL path.
func HandleRuntimeConfigKey(deps RuntimeDeps, key string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			cphttperr.WriteError(w, http.StatusMethodNotAllowed, "method not allowed", "method_not_allowed", "METHOD_NOT_ALLOWED")
			return
		}

		for _, k := range KnownRuntimeConfigKeys {
			if k == key {
				WriteJSON(w, http.StatusOK, map[string]any{
					"key":     k,
					"version": keyVersion(deps, k),
					"state":   snapshotFor(deps, k),
				})
				return
			}
		}
		cphttperr.WriteError(w, http.StatusNotFound, "unknown config key", "not_found", "NOT_FOUND")
	})
}

// HandleRuntimeSyncStatus returns the shadow-sync cursor and derived inSync
// flag. lastSyncAt is the RFC3339 timestamp of the most recent successful
// shadow_report, or empty when no report has been sent yet.
func HandleRuntimeSyncStatus(deps RuntimeDeps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			cphttperr.WriteError(w, http.StatusMethodNotAllowed, "method not allowed", "method_not_allowed", "METHOD_NOT_ALLOWED")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"desiredVer":  desiredVer(deps),
			"reportedVer": reportedVer(deps),
			"inSync":      inSync(deps),
			"lastSyncAt":  lastReportedAt(deps),
		})
	})
}

// HandleRuntimeHealth returns per-subsystem liveness. Overall status is
// "ok" when every check reports "ok", otherwise "degraded". The HTTP status
// is always 200 so scraping collectors can read the body on either outcome;
// /healthz remains the binary liveness probe for kube-style controllers.
func HandleRuntimeHealth(deps RuntimeDeps, checks HealthChecks) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			cphttperr.WriteError(w, http.StatusMethodNotAllowed, "method not allowed", "method_not_allowed", "METHOD_NOT_ALLOWED")
			return
		}
		statuses := map[string]string{}
		if checks.Run != nil {
			out := checks.Run(r.Context())
			if out != nil {
				statuses = out
			}
		}
		overall := "ok"
		for _, v := range statuses {
			if v != "ok" {
				overall = "degraded"
				break
			}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"status":     overall,
			"checks":     statuses,
			"thingId":    deps.ThingID,
			"reportedAt": lastReportedAt(deps),
		})
	})
}

// snapshotFor returns the JSON-safe state for a known key. Any secret
// masking is the responsibility of the configtypes value itself — handlers
// never hand raw secrets to callers.
func snapshotFor(deps RuntimeDeps, key string) any {
	switch key {
	case "killswitch":
		if deps.KillswitchSnap == nil {
			return interception.Killswitch{}
		}
		return deps.KillswitchSnap.Snapshot()
	case "exemptions":
		if deps.ExemptionSnap == nil {
			return identity.ActiveExemptions{Entries: []identity.ActiveExemption{}}
		}
		return deps.ExemptionSnap.Snapshot()
	}
	return nil
}

// keyVersion returns the per-key version from the thingclient, or 0 when
// no thingclient is wired.
func keyVersion(deps RuntimeDeps, key string) int64 {
	if deps.Thingclient == nil {
		return 0
	}
	return deps.Thingclient.KeyVersion(key)
}

// desiredVer returns the current desired shadow version, or 0 when no
// thingclient is wired.
func desiredVer(deps RuntimeDeps) int64 {
	if deps.Thingclient == nil {
		return 0
	}
	return deps.Thingclient.DesiredVer()
}

// reportedVer returns the current reported shadow version, or 0 when no
// thingclient is wired.
func reportedVer(deps RuntimeDeps) int64 {
	if deps.Thingclient == nil {
		return 0
	}
	return deps.Thingclient.ReportedVer()
}

// inSync returns true when the reported version has caught up to the
// desired version. When no thingclient is wired the proxy is treated as
// in-sync (0 >= 0) so the absence of shadow wiring doesn't flag health.
func inSync(deps RuntimeDeps) bool {
	if deps.Thingclient == nil {
		return true
	}
	return deps.Thingclient.ReportedVer() >= deps.Thingclient.DesiredVer()
}

// lastReportedAt returns the RFC3339 timestamp of the most recent successful
// shadow_report, or empty when no thingclient is wired / no report sent.
func lastReportedAt(deps RuntimeDeps) string {
	if deps.Thingclient == nil {
		return ""
	}
	return deps.Thingclient.LastReportedAt()
}
