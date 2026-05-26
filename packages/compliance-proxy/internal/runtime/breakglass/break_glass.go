package breakglass

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/interception"
)

// bgRequest is the JSON body accepted by PUT /runtime/config/{key}. State is
// raw so each key's specific shape (Killswitch, ActiveExemptions, etc.) is
// validated by the per-key ApplyBreakGlass branch after decoding.
type bgRequest struct {
	State  json.RawMessage `json:"state"`
	Reason string          `json:"reason"`
}

// bgResponse is the JSON body returned to the caller on success. Version is
// the proxy-local key version after the bump. PendingReport is true when the
// Hub delivery hit an error and the state was spooled to disk; the caller
// should treat this as "applied locally, Hub will catch up after reconnect".
type bgResponse struct {
	Key           string `json:"key"`
	Version       int64  `json:"version"`
	ReportStatus  string `json:"reportStatus"`
	PendingReport bool   `json:"pendingReport"`
}

// pendingBreakGlass is the on-disk shape for deferred Hub delivery. Serialized
// under <DataDir>/pending_break_glass.json via atomic rename. There is at most
// one pending entry at a time: a second break-glass on the same key while a
// prior report is still pending simply overwrites the pending buffer with the
// newer state. This matches Hub's reconciliation model — it always takes the
// latest reported state, older states are dead.
type pendingBreakGlass struct {
	ConfigKey    string          `json:"config_key"`
	KeyVersion   int64           `json:"key_version"`
	State        json.RawMessage `json:"state"`
	Reason       string          `json:"reason,omitempty"`
	SourceIP     string          `json:"source_ip,omitempty"`
	ActorTokenID string          `json:"actor_token_id,omitempty"`
}

// pendingBreakGlassFileName is the on-disk buffer consulted by the background
// retry loop (future work) and by restart-replay.
const pendingBreakGlassFileName = "pending_break_glass.json"

// State is the per-server break-glass state: event log, pending buffer
// path, Hub-aware version source, and reporter. Constructed by
// newBreakGlassState at server wire-up and passed into the PUT handler.
type State struct {
	log           *EventLog
	pendingMu     sync.Mutex
	pendingDir    string
	reporter      config.BreakGlassReporter
	versionSource config.BreakGlassVersionSource
}

// NewBreakGlassState returns nil when dataDir is empty — callers treat nil
// as "break-glass surface disabled" and return 503 from the PUT handler.
// versionSource may be nil in tests; a nil source falls back to starting at
// version 1, but production code must wire a live *thingclient.Client so Hub
// reconciliation does not silently drop the write.
func NewBreakGlassState(
	dataDir string,
	reporter config.BreakGlassReporter,
	versionSource config.BreakGlassVersionSource,
) *State {
	if dataDir == "" {
		return nil
	}
	return &State{
		log:           NewEventLog(dataDir),
		pendingDir:    dataDir,
		reporter:      reporter,
		versionSource: versionSource,
	}
}

// nextVersion returns the break-glass newVer per spec §5.2 step 3. When
// versionSource is nil (test-only fallback) it returns 1 so the first
// break-glass in a fresh test harness still has a sensible version.
func (s *State) nextVersion() int64 {
	if s.versionSource == nil {
		return 1
	}
	d, r := s.versionSource.DesiredVer(), s.versionSource.ReportedVer()
	hi := d
	if r > hi {
		hi = r
	}
	return hi + 1
}

// pendingPath returns the on-disk path for the pending buffer.
func (s *State) pendingPath() string {
	return filepath.Join(s.pendingDir, pendingBreakGlassFileName)
}

// writePending persists the pending entry via temp-file + rename so a crash
// mid-write cannot leave a partial JSON blob on disk.
func (s *State) writePending(p pendingBreakGlass) error {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()

	if err := os.MkdirAll(s.pendingDir, 0o750); err != nil {
		return fmt.Errorf("pending: mkdir: %w", err)
	}
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("pending: marshal: %w", err)
	}
	tmp := s.pendingPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return fmt.Errorf("pending: write temp: %w", err)
	}
	if err := os.Rename(tmp, s.pendingPath()); err != nil {
		return fmt.Errorf("pending: rename: %w", err)
	}
	return nil
}

// clearPending removes the pending buffer after a successful retry. Callers
// tolerate os.IsNotExist.
func (s *State) clearPending() error {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if err := os.Remove(s.pendingPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("pending: remove: %w", err)
	}
	return nil
}

// readPending returns the current pending entry and true, or a zero value
// and false when no pending file exists. Errors other than "not found" are
// surfaced so the caller can log them — a corrupt pending file should not
// be silently swallowed.
func (s *State) readPending() (pendingBreakGlass, bool, error) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()

	data, err := os.ReadFile(s.pendingPath())
	if err != nil {
		if os.IsNotExist(err) {
			return pendingBreakGlass{}, false, nil
		}
		return pendingBreakGlass{}, false, fmt.Errorf("pending: read: %w", err)
	}
	var p pendingBreakGlass
	if err := json.Unmarshal(data, &p); err != nil {
		return pendingBreakGlass{}, false, fmt.Errorf("pending: decode: %w", err)
	}
	return p, true, nil
}

// ReplayPending attempts to redeliver a buffered break-glass report to Hub.
// Returns (true, nil) when a pending entry was drained successfully,
// (false, nil) when there was nothing to replay or the reporter is nil, and
// (false, err) when the reporter errored. On reporter error the pending
// file is intentionally left in place so the next retry can try again.
//
// Safe to call concurrently with HandleBreakGlassPut: readPending /
// clearPending share s.pendingMu with writePending; the reporter call
// itself does not hold the lock so it does not block new break-glass
// writes from landing in their own pending slot.
func (s *State) ReplayPending(ctx context.Context) (bool, error) {
	if s == nil || s.reporter == nil {
		return false, nil
	}
	p, ok, err := s.readPending()
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if err := s.reporter.SendBreakGlassShadowReport(
		ctx, p.ConfigKey, p.State, p.KeyVersion, p.Reason, p.SourceIP, p.ActorTokenID,
	); err != nil {
		return false, fmt.Errorf("pending: replay: %w", err)
	}
	if err := s.clearPending(); err != nil {
		return false, fmt.Errorf("pending: clear after replay: %w", err)
	}
	return true, nil
}

// HandleBreakGlassPut is the PUT /runtime/config/{key} handler. Implements
// spec §5.2 atomically in the order:
//
//  1. Decode + validate request body.
//  2. Apply locally via the per-key ApplyBreakGlass / Rebuild hook.
//  3. newVer = max(versionSource.DesiredVer, versionSource.ReportedVer) + 1.
//  4. Append BreakGlassEvent to the JSONL log and fsync — if this fails we
//     return 500 without attempting Hub delivery, because a successful reply
//     must be backed by a durable local record.
//  5. Best-effort Hub shadow_report; on failure spool to pending_break_glass.json.
//
// The log append MUST happen before shadow_report so a crash between the two
// still leaves a durable audit of what was applied locally. The response
// body's reportStatus field carries the delivery outcome back to the caller
// — it is NOT persisted to the log (the log's job is audit, not delivery
// bookkeeping).
func HandleBreakGlassPut(deps handler.RuntimeDeps, key string, bg *State) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bg == nil {
			http.Error(w, `{"error":"break-glass disabled (no data dir configured)"}`, http.StatusServiceUnavailable)
			return
		}
		if r.Method != http.MethodPut {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		// --- Step 1: decode + validate ---
		var req bgRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
			return
		}
		if len(req.State) == 0 || bytes.Equal(bytes.TrimSpace(req.State), []byte("null")) {
			http.Error(w, `{"error":"state is required"}`, http.StatusBadRequest)
			return
		}

		// --- Step 2: apply locally ---
		if err := applyBreakGlassLocal(deps, key, req.State); err != nil {
			http.Error(w,
				`{"error":"`+escapeBGError(err.Error())+`"}`,
				http.StatusBadRequest)
			return
		}

		// --- Step 3: compute newVer from Hub-known versions ---
		version := bg.nextVersion()

		// --- Step 4: durable event log (BEFORE shadow_report) ---
		evt := BreakGlassEvent{
			At:           time.Now().UTC(),
			ConfigKey:    key,
			KeyVersion:   version,
			State:        req.State,
			Reason:       req.Reason,
			SourceIP:     clientSourceIP(r),
			ActorTokenID: actorTokenID(r),
		}
		if err := bg.log.Append(evt); err != nil {
			// Durable log failure: the apply already happened, but we cannot
			// prove it to the operator. Surface a 500 so they see the failure
			// and page the on-call before we attempt Hub delivery.
			http.Error(w,
				`{"error":"break-glass applied but event log failed: `+escapeBGError(err.Error())+`"}`,
				http.StatusInternalServerError)
			return
		}

		// --- Step 5: best-effort shadow_report ---
		reportStatus := "skipped"
		pending := false
		if bg.reporter != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			err := bg.reporter.SendBreakGlassShadowReport(
				ctx, key, req.State, version, req.Reason, evt.SourceIP, evt.ActorTokenID,
			)
			cancel()
			if err == nil {
				reportStatus = "ok"
				// A successful report retires any stale pending entry.
				_ = bg.clearPending()
			} else {
				reportStatus = "pending"
				pending = true
				if werr := bg.writePending(pendingBreakGlass{
					ConfigKey:    key,
					KeyVersion:   version,
					State:        req.State,
					Reason:       req.Reason,
					SourceIP:     evt.SourceIP,
					ActorTokenID: evt.ActorTokenID,
				}); werr != nil && deps.Logger != nil {
					deps.Logger.Error("break-glass: failed to spool pending report",
						"key", key, "version", version, "error", werr)
				}
				if deps.Logger != nil {
					deps.Logger.Warn("break-glass: Hub delivery failed, spooled to pending",
						"key", key, "version", version, "error", err)
				}
			}
		}

		config.WriteJSON(w, http.StatusOK, bgResponse{
			Key:           key,
			Version:       version,
			ReportStatus:  reportStatus,
			PendingReport: pending,
		})
	})
}

// applyBreakGlassLocal dispatches to the per-key apply hook. Each branch
// decodes the incoming state against the canonical configtypes shape so a
// malformed payload fails with 400 before the event log runs.
func applyBreakGlassLocal(deps handler.RuntimeDeps, key string, state json.RawMessage) error {
	switch key {
	case "killswitch":
		if deps.KillswitchSnap == nil {
			return fmt.Errorf("killswitch surface not configured")
		}
		var ks interception.Killswitch
		if err := json.Unmarshal(state, &ks); err != nil {
			return fmt.Errorf("invalid killswitch payload: %w", err)
		}
		return deps.KillswitchSnap.ApplyBreakGlass(ks)

	case "exemptions":
		if deps.ExemptionRebuilder == nil {
			return fmt.Errorf("exemptions surface not configured")
		}
		return handler.ApplyActiveExemptions(deps.ExemptionRebuilder, state)

	default:
		// Only killswitch and exemptions are writable on the
		// break-glass surface. Every other shadow key flows from the
		// control-plane admin API; operators should use the admin UI.
		return fmt.Errorf("break-glass write not supported for key %q", key)
	}
}

// clientSourceIP extracts the remote address from the request. Trusts
// X-Forwarded-For when present (runtime API is bound to loopback; the BFF
// sets this header).
func clientSourceIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		// Take the first entry (original client).
		for i, c := range v {
			if c == ',' {
				return v[:i]
			}
		}
		return v
	}
	return r.RemoteAddr
}

// actorTokenID extracts the admin token id from the forwarded header set by
// the BFF. When absent (direct hits bypassing the BFF) the field falls back
// to "api" so the event log still distinguishes break-glass from shadow
// applies.
func actorTokenID(r *http.Request) string {
	if v := r.Header.Get("X-Nexus-Actor-Token-Id"); v != "" {
		return v
	}
	return "api"
}

// escapeBGError applies the minimum escaping needed to embed a Go error
// string inside a hand-built JSON literal.
func escapeBGError(s string) string {
	out := make([]byte, 0, len(s))
	for i := range len(s) {
		c := s[i]
		if c == '\\' || c == '"' {
			out = append(out, '\\', c)
			continue
		}
		out = append(out, c)
	}
	return string(out)
}
