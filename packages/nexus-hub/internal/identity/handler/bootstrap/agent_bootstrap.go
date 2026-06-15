package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// AgentBootstrapHandler serves GET /api/public/agent-bootstrap, which
// returns the deployment-wide settings an agent needs to start the SSO
// self-enrollment flow:
//
//   - controlPlaneURL: where to POST /api/agent/sso-enroll (operator-set
//     in Hub config, deliberately not per-device so a fresh install only
//     needs the Hub URL the installer already configured).
//   - deviceAuthMode: the current "mtls-only" / "enterprise-login"
//     setting (read live from system_metadata so toggling it in the CP
//     UI takes effect without a Hub restart).
//
// Unauthenticated by design — pre-enrollment agents have no client
// cert. Returned fields contain no secrets. Cached for 60s to bound DB
// load under boot-storm conditions.
//
// DB is typed as the store.PgxPool interface so tests can inject pgxmock;
// *pgxpool.Pool satisfies it in production. Mirrors the seam used by Store
// + Manager + RuntimeBridgeAPI + DiagDrainAPI.
type AgentBootstrapHandler struct {
	CpURL string
	DB    store.PgxPool

	cache atomic.Pointer[bootstrapCacheEntry]
	mu    sync.Mutex
}

type bootstrapCacheEntry struct {
	body    []byte
	fetched time.Time
}

const bootstrapCacheTTL = 60 * time.Second

// bootstrapResponse is the JSON shape returned to agents. Field names
// match the public contract documented in
// docs/developers/architecture/services/hub/nexus-hub-enrollment-architecture.md.
type bootstrapResponse struct {
	ControlPlaneURL string `json:"controlPlaneURL"`
	DeviceAuthMode  string `json:"deviceAuthMode"`
}

// normaliseAgentBootstrapMode collapses `local-login` to
// `enterprise-login` for the public bootstrap response. The raw value
// stays in `system_metadata` (admin / audit visibility); only the
// agent-facing surface normalises so existing agents can drive both
// device-auth modes through their single browser-SSO branch without a
// binary rebuild. Other values pass through unchanged.
func normaliseAgentBootstrapMode(rawMode string) string {
	if rawMode == "local-login" {
		return "enterprise-login"
	}
	return rawMode
}

// Handle implements echo.HandlerFunc.
func (h *AgentBootstrapHandler) Handle(c echo.Context) error {
	body, err := h.body(c.Request().Context())
	if err != nil {
		// Canonical {error, code} envelope via the package error helper.
		return internalError(c, err.Error())
	}
	return c.JSONBlob(http.StatusOK, body)
}

func (h *AgentBootstrapHandler) body(ctx context.Context) ([]byte, error) {
	if entry := h.cache.Load(); entry != nil && time.Since(entry.fetched) < bootstrapCacheTTL {
		return entry.body, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if entry := h.cache.Load(); entry != nil && time.Since(entry.fetched) < bootstrapCacheTTL {
		return entry.body, nil
	}

	mode := "mtls-only"
	if h.DB != nil {
		var raw []byte
		err := h.DB.QueryRow(ctx,
			`SELECT value FROM system_metadata WHERE key = $1`,
			"device.auth.mode",
		).Scan(&raw)
		if err == nil && len(raw) > 0 {
			var parsed string
			if json.Unmarshal(raw, &parsed) == nil && parsed != "" {
				mode = parsed
			}
		} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			// Fall through with the safe default; caller logs via
			// the Echo error handler if we return non-nil.
			return nil, err
		}
	}

	mode = normaliseAgentBootstrapMode(mode)

	resp := bootstrapResponse{
		ControlPlaneURL: h.CpURL,
		DeviceAuthMode:  mode,
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	h.cache.Store(&bootstrapCacheEntry{body: body, fetched: time.Now()})
	return body, nil
}
