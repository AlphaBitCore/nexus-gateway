package ws

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/coder/websocket"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/agentca"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
)

// thingManager is the slice of *manager.Manager the WS server actually uses.
// Declared as an interface so tests can inject a fake without standing up a
// live Postgres + Manager wiring. Production callers still pass the concrete
// *manager.Manager constructed in cmd/nexus-hub/main.go — the interface is
// implemented automatically.
type thingManager interface {
	RegisterThing(ctx context.Context, req manager.RegisterRequest) (*manager.RegisterResponse, error)
	HandleShadowReport(ctx context.Context, req manager.ShadowReportRequest) error
	TouchLiveness(ctx context.Context, thingID string)
	MarkOffline(ctx context.Context, thingID string)
}

// tokenValidator is the device-token validation surface authenticate() uses.
// Implemented by *store.Store; tests inject a fake to exercise the device
// token success/failure branches without a Postgres dependency.
type tokenValidator interface {
	ValidateDeviceToken(ctx context.Context, thingID, tokenHash string) (*store.Thing, error)
}

// OpsMetricsHandler is the dispatch surface ws.Server uses to forward
// metrics_sample and diag_event WS messages to the Hub-side opsmetrics
// pipeline. The concrete implementation lives in
// packages/nexus-hub/internal/observability/opsmetrics; keeping the interface here breaks
// the import cycle that would otherwise form (opsmetrics already depends on
// shared/opsmetrics types and this package only needs the dispatch shape).
//
// Both methods MUST be non-blocking from the caller's perspective —
// implementations enqueue onto bounded channels and return nil for normal
// drops; only hard misuse (parse failure, nil writer) returns an error.
type OpsMetricsHandler interface {
	HandleMetricsSample(ctx context.Context, thingID, thingType string, raw json.RawMessage) error
	HandleDiagEvent(ctx context.Context, thingID, thingType string, raw json.RawMessage) error
	// HandleStaticInfo persists a flat static_info envelope (spec §5.6 / §6.2)
	// into thing.metadata.staticInfo. Called from the WS read pump on receipt
	// of a Thing-emitted static_info message; thingID/thingType come from the
	// authenticated WS identity, not the payload.
	HandleStaticInfo(ctx context.Context, thingID, thingType string, raw json.RawMessage) error
}

// Server handles WebSocket upgrades, authentication, and message dispatch.
type Server struct {
	pool           *Pool
	mgr            thingManager
	validator      tokenValidator
	hubID          string
	serviceToken   string
	allowedOrigins []string
	ops            OpsMetricsHandler
	logger         *slog.Logger
}

// NewServer creates a WebSocket server. allowedOrigins is the production
// origin allowlist (e.g. cluster DNS names) appended to the built-in
// localhost patterns; it may be nil or empty for dev/test setups.
func NewServer(pool *Pool, mgr *manager.Manager, hubID, serviceToken string, allowedOrigins []string, logger *slog.Logger) *Server {
	var validator tokenValidator
	if st := mgr.Store(); st != nil {
		validator = st.RegistryStore()
	}
	return newServerWithDeps(pool, mgr, validator, hubID, serviceToken, allowedOrigins, logger)
}

// newServerWithDeps is the dependency-injection seam used by NewServer in
// production and by tests that want to substitute a fake manager / token
// validator. The seam exists for unit-test reachability of authenticate,
// HandleUpgrade, and handleMessage without a live Postgres.
func newServerWithDeps(pool *Pool, mgr thingManager, validator tokenValidator, hubID, serviceToken string, allowedOrigins []string, logger *slog.Logger) *Server {
	return &Server{
		pool:           pool,
		mgr:            mgr,
		validator:      validator,
		hubID:          hubID,
		serviceToken:   serviceToken,
		allowedOrigins: allowedOrigins,
		logger:         logger.With("component", "ws_server"),
	}
}

// SetOpsMetricsHandler wires the opsmetrics dispatch target. Called by main
// after the writers are constructed. If unset (e.g. test harnesses that do
// not exercise opsmetrics), metrics_sample / diag_event messages are
// silently dropped at the ws layer.
func (s *Server) SetOpsMetricsHandler(h OpsMetricsHandler) { s.ops = h }

// HandleUpgrade handles the HTTP → WebSocket upgrade.
func (s *Server) HandleUpgrade(w http.ResponseWriter, r *http.Request) {
	thingID, thingType, err := s.authenticate(r)
	if err != nil {
		s.logger.Warn("ws authenticate failed", "error", err, "remote_addr", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Hub WebSocket is service-to-service only (agents, CP, AI gateway,
	// compliance proxy). Allow localhost for dev and the configured
	// production allowlist for cluster traffic; reject browser origins so
	// a malicious page cannot hijack a user's bearer token into the Hub.
	originPatterns := append([]string{"localhost", "localhost:*", "127.0.0.1", "127.0.0.1:*"}, s.allowedOrigins...)
	wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: originPatterns,
		Subprotocols:   []string{"nexus.bearer"},
	})
	if err != nil {
		s.logger.Error("ws accept failed", "error", err)
		return
	}

	// Register Thing. The handshake URL carries the same fields the HTTP
	// register fallback sends as a JSON body — pull them off the query
	// string so the thing row has populated version / address / metrics_url
	// / role and the runtime-bridge introspection endpoint can resolve a
	// reachable URL. Without these the row lands with NULLs and the
	// "/api/admin/nodes/<id>/runtime" call fails with
	// "thing has no metrics_url registered".
	q := r.URL.Query()
	regResp, err := s.mgr.RegisterThing(r.Context(), manager.RegisterRequest{
		ID:            thingID,
		Type:          thingType,
		Name:          q.Get("name"),
		Version:       q.Get("version"),
		Address:       q.Get("address"),
		MetricsURL:    q.Get("metricsUrl"),
		ManagementURL: q.Get("managementUrl"),
		Role:          q.Get("role"),
		RuntimeAPIURL: q.Get("runtimeApiUrl"),
		PhysicalID:    q.Get("physicalId"),
	})
	if err != nil {
		s.logger.Error("registration on ws connect failed", "thing_id", thingID, "error", err)
		_ = wsConn.Close(websocket.StatusInternalError, "registration failed")
		return
	}

	onLiveness := func(id string) {
		s.mgr.TouchLiveness(context.Background(), id)
	}
	conn := newConn(wsConn, thingID, thingType, s.handleMessage, onLiveness, s.logger)
	s.pool.Add(conn)

	// Send connected message
	connMsg := ConnectedMessage{
		Type:       "connected",
		HubID:      s.hubID,
		Desired:    regResp.Desired,
		DesiredVer: regResp.DesiredVer,
	}
	if data, err := json.Marshal(connMsg); err == nil {
		// best-effort: handshake reply; if the write fails, conn.Run below
		// will surface the broken connection on the next read/write.
		_ = conn.Write(data)
	}

	s.logger.Info("ws connected", "thing_id", thingID, "thing_type", thingType)

	// Run blocks until disconnect
	conn.Run(r.Context())

	// Cleanup on disconnect
	s.pool.Remove(conn)
	s.mgr.MarkOffline(context.Background(), thingID)
	s.logger.Info("ws disconnected", "thing_id", thingID)
}

var errUnauthorized = errors.New("unauthorized")

func (s *Server) authenticate(r *http.Request) (thingID, thingType string, err error) {
	token := extractBearerToken(r)
	if token == "" {
		return "", "", errUnauthorized
	}

	// Path 1: internal service token (CP, other services).
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.serviceToken)) == 1 {
		thingID = r.URL.Query().Get("id")
		thingType = r.URL.Query().Get("type")
		if thingID == "" || thingType == "" {
			return "", "", errUnauthorized
		}
		return thingID, thingType, nil
	}

	qID := r.URL.Query().Get("id")
	if qID == "" {
		return "", "", errUnauthorized
	}

	// Path 2: device token (hashed, stored in thing.metadata.deviceTokenHash).
	tokenHash, hashErr := agentca.HashDeviceToken(token)
	if hashErr != nil {
		return "", "", fmt.Errorf("hash token: %w", hashErr)
	}
	thing, err := s.validator.ValidateDeviceToken(r.Context(), qID, tokenHash)
	if err != nil {
		return "", "", errUnauthorized
	}
	return thing.ID, thing.Type, nil
}

func (s *Server) handleMessage(thingID, thingType string, data []byte) {
	msg, err := ParseIncoming(data)
	if err != nil {
		s.logger.Warn("invalid ws message", "thing_id", thingID, "error", err)
		return
	}

	ctx := context.Background()

	switch msg.Type {
	case "shadow_report":
		var payload ShadowReportPayload
		if err := json.Unmarshal(msg.Raw, &payload); err != nil {
			s.logger.Warn("invalid shadow_report", "thing_id", thingID, "error", err)
			return
		}
		if err := s.mgr.HandleShadowReport(ctx, manager.ShadowReportRequest{
			ID:                  thingID,
			Reported:            payload.Reported,
			ReportedVer:         payload.ReportedVer,
			ReportedOutcomesRaw: payload.ReportedOutcomes,
		}); err != nil {
			s.logger.Error("shadow report failed", "thing_id", thingID, "error", err)
		}

	case "metrics_sample":
		// thingID/thingType are the WS-authenticated identity captured at
		// upgrade; the payload's own thingId field is informational and
		// must not influence routing. Per spec §7.1 the envelope is flat,
		// so the raw bytes (which include "type") deserialize cleanly into
		// SampleBatch via the embedded JSON tags.
		if s.ops == nil {
			s.logger.Debug("opsmetrics handler not configured, dropping metrics_sample",
				"thing_id", thingID)
			return
		}
		if err := s.ops.HandleMetricsSample(ctx, thingID, thingType, msg.Raw); err != nil {
			s.logger.Warn("metrics_sample dispatch failed",
				"thing_id", thingID,
				"error", err)
		}

	case "diag_event":
		if s.ops == nil {
			s.logger.Debug("opsmetrics handler not configured, dropping diag_event",
				"thing_id", thingID)
			return
		}
		if err := s.ops.HandleDiagEvent(ctx, thingID, thingType, msg.Raw); err != nil {
			s.logger.Warn("diag_event dispatch failed",
				"thing_id", thingID,
				"error", err)
		}

	case "static_info":
		if s.ops == nil {
			s.logger.Debug("opsmetrics handler not configured, dropping static_info",
				"thing_id", thingID)
			return
		}
		if err := s.ops.HandleStaticInfo(ctx, thingID, thingType, msg.Raw); err != nil {
			s.logger.Warn("static_info dispatch failed",
				"thing_id", thingID,
				"error", err)
		}

	default:
		s.logger.Debug("unknown ws message type", "thing_id", thingID, "type", msg.Type)
	}
}

// extractBearerToken returns the bearer token from either the Authorization
// header or the Sec-WebSocket-Protocol subprotocol negotiation. The query-
// parameter fallback was removed because URLs frequently appear in proxy
// access logs.
func extractBearerToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		const prefix = "Bearer "
		if strings.HasPrefix(auth, prefix) {
			return strings.TrimSpace(auth[len(prefix):])
		}
		return ""
	}
	// Subprotocol form: "nexus.bearer, <token>"
	for _, proto := range r.Header.Values("Sec-WebSocket-Protocol") {
		parts := strings.Split(proto, ",")
		if len(parts) == 2 && strings.TrimSpace(parts[0]) == "nexus.bearer" {
			return strings.TrimSpace(parts[1])
		}
	}
	return ""
}

// Pool returns the connection pool (for external access).
func (s *Server) Pool() *Pool { return s.pool }

// Close closes all connections.
func (s *Server) Close() { s.pool.CloseAll() }
