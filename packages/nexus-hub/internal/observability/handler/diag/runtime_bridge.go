package diag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
)

// RuntimeBridgeAPI proxies CP runtime introspection requests to the Thing's
// /debug/runtime endpoint and wraps the response with Hub-side meta
// (desired_ver, reported_ver, last_seen_at, etc.) so the UI can diff
// "what Hub thinks is desired" vs "what the Thing has applied".
//
// DB is typed as the store.PgxPool interface so tests can inject pgxmock;
// *pgxpool.Pool satisfies the same surface in production. Mirrors the
// seam already used by Store + Manager.
type RuntimeBridgeAPI struct {
	DB           store.PgxPool
	ServiceToken string
	HubID        string
	HubLocalURL  string // e.g. http://localhost:3060 — used when target id == HubID
	HTTPClient   *http.Client
}

// Meta surfaces Hub's view of the Thing alongside the snapshot so the UI
// can diff desired vs reported vs applied in three columns.
type runtimeBridgeMeta struct {
	ThingID     string          `json:"thing_id"`
	ThingType   string          `json:"thing_type"`
	ThingStatus string          `json:"thing_status"`
	DesiredVer  int64           `json:"desired_ver"`
	ReportedVer int64           `json:"reported_ver"`
	LastSeenAt  *time.Time      `json:"last_seen_at,omitempty"`
	Desired     json.RawMessage `json:"desired,omitempty"`
	Reported    json.RawMessage `json:"reported,omitempty"`
}

// Runtime handles GET /api/hub/things/:id/runtime.
func (a *RuntimeBridgeAPI) Runtime(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "id is required"})
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), 10*time.Second)
	defer cancel()

	meta, metricsURL, err := a.fetchMeta(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, map[string]string{"error": "thing not found"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if meta.ThingType == "agent" {
		return c.JSON(http.StatusNotImplemented, map[string]any{
			"error": "agent introspection not exposed via Hub bridge (NAT) — see e31-s12 for the local agent UI path",
			"meta":  meta,
		})
	}
	if meta.ThingStatus != "online" {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{
			"error": fmt.Sprintf("thing status %q (need online)", meta.ThingStatus),
			"meta":  meta,
		})
	}

	target, err := resolveIntrospectURL(meta.ThingID, a.HubID, a.HubLocalURL, metricsURL)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{
			"error": err.Error(),
			"meta":  meta,
		})
	}

	body, status, err := a.fetchSnapshot(ctx, target)
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]any{
			"error":  fmt.Sprintf("reverse call failed: %v", err),
			"target": target,
			"meta":   meta,
		})
	}
	if status != http.StatusOK {
		return c.JSON(http.StatusBadGateway, map[string]any{
			"error":        "thing returned non-200",
			"thing_status": status,
			"thing_body":   string(body),
			"target":       target,
			"meta":         meta,
		})
	}

	return c.JSON(http.StatusOK, map[string]any{
		"snapshot": json.RawMessage(body),
		"meta":     meta,
	})
}

func (a *RuntimeBridgeAPI) fetchMeta(ctx context.Context, id string) (*runtimeBridgeMeta, string, error) {
	row := a.DB.QueryRow(ctx, `
		SELECT t.id, t.type, t.status, t.desired_ver, t.reported_ver,
		       t.last_seen_at, t.desired, t.reported,
		       COALESCE(ts.metrics_url, '')
		FROM thing t
		LEFT JOIN thing_service ts ON ts.thing_id = t.id
		WHERE t.id = $1
	`, id)

	var (
		m          runtimeBridgeMeta
		metricsURL string
	)
	if err := row.Scan(
		&m.ThingID, &m.ThingType, &m.ThingStatus,
		&m.DesiredVer, &m.ReportedVer,
		&m.LastSeenAt, &m.Desired, &m.Reported,
		&metricsURL,
	); err != nil {
		return nil, "", err
	}
	return &m, metricsURL, nil
}

func (a *RuntimeBridgeAPI) fetchSnapshot(ctx context.Context, url string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+a.ServiceToken)
	req.Header.Set("Accept", "application/json")

	client := a.HTTPClient
	if client == nil {
		client = nexushttp.New(nexushttp.Config{
			Timeout:        8 * time.Second,
			Caller:         "hub-runtime-bridge",
			PropagateReqID: true,
		})
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// resolveIntrospectURL converts a thing.metrics_url (e.g. "http://host:9100/metrics")
// into the corresponding /debug/runtime URL. For the hub itself we prefer
// hubLocalURL so the self-call avoids a round-trip via the advertised host.
func resolveIntrospectURL(thingID, hubID, hubLocalURL, metricsURL string) (string, error) {
	if thingID == hubID && hubLocalURL != "" {
		return strings.TrimRight(hubLocalURL, "/") + "/debug/runtime", nil
	}
	if metricsURL == "" {
		return "", errors.New("thing has no metrics_url registered")
	}
	base := strings.TrimSuffix(metricsURL, "/metrics")
	base = strings.TrimRight(base, "/")
	return base + "/debug/runtime", nil
}
