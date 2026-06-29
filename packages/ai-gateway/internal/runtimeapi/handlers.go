package runtimeapi

import (
	"github.com/goccy/go-json"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// configEntry is the wire shape of a single shadow-managed config key.
// State is forwarded from thingclient.ConfigState.State verbatim — it is a
// json.RawMessage so the raw shadow payload surfaces to the caller unchanged.
type configEntry struct {
	Version int64           `json:"version"`
	State   json.RawMessage `json:"state"`
}

func (s *Server) handleRuntimeConfig(w http.ResponseWriter, _ *http.Request) {
	snap := s.thing.SnapshotDesired()
	out := make(map[string]configEntry, len(snap))
	for k, cs := range snap {
		out[k] = configEntry{Version: cs.Version, State: cs.State}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"thing_version":    s.thing.DesiredVer(),
		"reported_version": s.thing.ReportedVer(),
		"last_reported_at": s.thing.LastReportedAt(),
		"config":           out,
	})
}

func (s *Server) handleRuntimeConfigKey(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	snap := s.thing.SnapshotDesired()
	cs, ok := snap[key]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, configEntry{Version: cs.Version, State: cs.State})
}

func (s *Server) handleRuntimeSyncStatus(w http.ResponseWriter, _ *http.Request) {
	dv, rv := s.thing.DesiredVer(), s.thing.ReportedVer()
	writeJSON(w, http.StatusOK, map[string]any{
		"in_sync":          dv == rv,
		"thing_version":    dv,
		"reported_version": rv,
		"last_reported_at": s.thing.LastReportedAt(),
	})
}

func (s *Server) handleRuntimeHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"thing_version":    s.thing.DesiredVer(),
		"reported_version": s.thing.ReportedVer(),
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ThingSnapshotter is the subset of *thingclient.Client that the runtimeapi depends on.
// An interface keeps the package unit-testable with a fake.
type ThingSnapshotter interface {
	SnapshotDesired() map[string]thingclient.ConfigState
	DesiredVer() int64
	ReportedVer() int64
	KeyVersion(string) int64
	LastReportedAt() string
}
