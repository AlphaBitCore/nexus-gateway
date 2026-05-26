package thingclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// httpClient wraps net/http.Client with Hub-specific auth and base URL.
type httpClient struct {
	client  *http.Client
	baseURL string
	token   string
	thingID string
	logger  *slog.Logger
}

func newHTTPClient(baseURL, token, thingID string, logger *slog.Logger) *httpClient {
	return &httpClient{
		client: nexushttp.New(nexushttp.Config{
			Timeout:        10 * time.Second,
			Caller:         "thingclient-http",
			PropagateReqID: true,
		}),
		baseURL: baseURL,
		token:   token,
		thingID: thingID,
		logger:  logger,
	}
}

// do executes an HTTP request with Bearer auth and returns the response body.
// Always emits X-Thing-Id when present — Hub's device-token auth path
// rejects requests without it (HTTP 401 "X-Thing-Id header required for
// device token auth") and the audit upload queue backs up until the
// client reconnects with the correct header.
func (h *httpClient) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	url := h.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+h.token)
	if h.thingID != "" {
		req.Header.Set("X-Thing-Id", h.thingID)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	return respBody, resp.StatusCode, nil
}

// --- Request/Response types ---

type registerRequest struct {
	ID            string `json:"id"`
	Type          string `json:"type"`
	Name          string `json:"name,omitempty"`
	Version       string `json:"version,omitempty"`
	Address       string `json:"address,omitempty"`
	RuntimeAPIURL string `json:"runtimeApiUrl,omitempty"`
	MetricsURL    string `json:"metricsUrl,omitempty"`
	ManagementURL string `json:"managementUrl,omitempty"`
	Role          string `json:"role,omitempty"`
	PhysicalID    string `json:"physicalId,omitempty"`
}

type registerResponse struct {
	ThingID    string                 `json:"thingId"`
	Desired    map[string]ConfigState `json:"desired"`
	DesiredVer int64                  `json:"desiredVer"`
}

type heartbeatRequest struct {
	ID          string         `json:"id"`
	Status      string         `json:"status"`
	ReportedVer int64          `json:"reportedVer"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

type heartbeatResponse struct {
	Ack        bool                   `json:"ack"`
	DesiredVer int64                  `json:"desiredVer"`
	Desired    map[string]ConfigState `json:"desired,omitempty"`
}

type shadowRequest struct {
	ID string `json:"id"`
	// Reported is a map of configKey → raw state (no {state, version}
	// wrapper). See thingMessage.Reported for the rationale.
	Reported    map[string]json.RawMessage `json:"reported"`
	ReportedVer int64                      `json:"reportedVer"`
	// ReportedOutcomes mirrors thingMessage.ReportedOutcomes so the HTTP
	// fallback path is byte-for-byte compatible with the WS shadow_report
	// payload — Hub-side parsing is the same code path either way.
	ReportedOutcomes map[string]ApplyOutcome `json:"reportedOutcomes,omitempty"`
}

type configPullResponse struct {
	Configs    map[string]ConfigState `json:"configs"`
	DesiredVer int64                  `json:"desiredVer"`
}

// --- HTTP methods ---

func (c *Client) httpRegister(ctx context.Context) (*registerResponse, error) {
	req := registerRequest{
		ID:            c.cfg.ThingID,
		Type:          c.cfg.ThingType,
		Name:          c.cfg.ThingName,
		Version:       c.cfg.ThingVersion,
		Address:       c.cfg.ListenAddress,
		RuntimeAPIURL: c.cfg.RuntimeAPIURL,
		MetricsURL:    c.cfg.MetricsURL,
		ManagementURL: c.cfg.ManagementURL,
		Role:          c.cfg.Role,
		PhysicalID:    c.cfg.PhysicalID,
	}

	hc := c.getHTTPClient()
	body, status, err := hc.do(ctx, http.MethodPost, "/api/internal/things/register", req)
	if err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("register: HTTP %d: %s", status, string(body))
	}

	c.promMetrics.httpFallbackReqs.WithLabelValues("register").Inc()

	var resp registerResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("register: unmarshal response: %w", err)
	}

	return &resp, nil
}

func (c *Client) httpHeartbeat(ctx context.Context) (*heartbeatResponse, error) {
	req := heartbeatRequest{
		ID:          c.cfg.ThingID,
		Status:      "online",
		ReportedVer: c.reportedVer.Load(),
	}

	hc := c.getHTTPClient()
	body, status, err := hc.do(ctx, http.MethodPost, "/api/internal/things/heartbeat", req)
	if err != nil {
		return nil, fmt.Errorf("heartbeat: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("heartbeat: HTTP %d: %s", status, string(body))
	}

	c.promMetrics.httpFallbackReqs.WithLabelValues("heartbeat").Inc()

	var resp heartbeatResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("heartbeat: unmarshal response: %w", err)
	}

	return &resp, nil
}

func (c *Client) httpShadowReport(ctx context.Context, reported map[string]ConfigState, ver int64) error {
	req := shadowRequest{
		ID:               c.cfg.ThingID,
		Reported:         flattenReported(reported),
		ReportedVer:      ver,
		ReportedOutcomes: c.outcomes.Snapshot(),
	}

	hc := c.getHTTPClient()
	body, status, err := hc.do(ctx, http.MethodPost, "/api/internal/things/shadow", req)
	if err != nil {
		return fmt.Errorf("shadow report: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("shadow report: HTTP %d: %s", status, string(body))
	}

	c.promMetrics.httpFallbackReqs.WithLabelValues("shadow").Inc()
	return nil
}

func (c *Client) httpConfigPull(ctx context.Context) (*configPullResponse, error) {
	path := fmt.Sprintf(
		"/api/internal/things/config?type=%s&id=%s",
		url.QueryEscape(c.cfg.ThingType),
		url.QueryEscape(c.cfg.ThingID),
	)
	hc := c.getHTTPClient()
	body, status, err := hc.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("config pull: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("config pull: HTTP %d: %s", status, string(body))
	}

	c.promMetrics.httpFallbackReqs.WithLabelValues("config_pull").Inc()

	var resp configPullResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("config pull: unmarshal response: %w", err)
	}

	return &resp, nil
}

func (c *Client) httpDeregister(ctx context.Context) {
	type deregisterReq struct {
		ID string `json:"id"`
	}

	hc := c.getHTTPClient()
	_, _, err := hc.do(ctx, http.MethodPost, "/api/internal/things/deregister", deregisterReq{
		ID: c.cfg.ThingID,
	})
	if err != nil {
		c.logger.Warn("Failed to deregister via HTTP",
			slog.String("event", "deregister_failed"),
			slog.String("error", err.Error()),
		)
		return
	}

	c.promMetrics.httpFallbackReqs.WithLabelValues("deregister").Inc()
	c.logger.Info("Deregistered from Hub via HTTP",
		slog.String("event", "deregistered"),
	)
}

func (c *Client) getHTTPClient() *httpClient {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hc == nil {
		baseURL := c.cfg.HubHTTPURL
		if baseURL == "" {
			baseURL = deriveHTTPURL(c.cfg.HubURL)
		}
		c.hc = newHTTPClient(baseURL, c.cfg.Token, c.cfg.ThingID, c.logger)
	}
	return c.hc
}

// --- HTTP Fallback Run Loop ---

// runHTTPFallback runs the HTTP polling loop until WebSocket recovery or shutdown.
func (c *Client) runHTTPFallback(ctx context.Context) {
	c.setMode(ModeHTTPFallback)

	resp, err := c.httpRegister(ctx)
	if err != nil {
		c.logger.Error("HTTP register failed",
			slog.String("event", "http_register_failed"),
			slog.String("error", err.Error()),
		)
		return
	}

	c.logger.Info("Registered with Hub via HTTP fallback",
		slog.String("event", "http_registered"),
		slog.Int64("desired_ver", resp.DesiredVer),
	)

	if len(resp.Desired) > 0 {
		c.recordKeyVersions(resp.Desired)
	}
	if resp.DesiredVer > c.reportedVer.Load() {
		c.desiredVer.Store(resp.DesiredVer)
		c.applyConfig(resp.Desired, resp.DesiredVer)
	}

	// Heartbeat cadence comes from CurrentHeartbeatInterval, which honors
	// SetHeartbeatInterval overrides. Each iteration re-arms a Timer;
	// SetHeartbeatInterval broadcasts on heartbeatKick so an in-flight
	// wait wakes early instead of running out the old interval.
	heartbeatTimer := time.NewTimer(c.CurrentHeartbeatInterval())
	defer heartbeatTimer.Stop()
	armHeartbeat := func() {
		heartbeatTimer.Reset(c.CurrentHeartbeatInterval())
	}

	// WS recovery uses exponential backoff (not a fixed-interval ticker) so we
	// don't hammer the Hub at a constant rate while it is down. recoveryFailures
	// is a local counter fed to calculateBackoffFor; it's distinct from
	// c.wsConsecutiveFailures (which drives the outer runLoop backoff) so that
	// the fallback loop's retry cadence grows even though we already switched
	// to HTTP and the outer counter is not being incremented here.
	recoveryFailures := 0

	// dial resolves the WS dialer, honoring the connectWSFn test seam.
	dial := c.connectWS
	if c.connectWSFn != nil {
		dial = c.connectWSFn
	}

	for {
		wsRetryDelay := c.calculateBackoffFor(recoveryFailures + 1)
		kickPtr := c.heartbeatKick.Load()
		select {
		case <-ctx.Done():
			return

		case <-*kickPtr:
			// Heartbeat interval changed; stop the in-flight timer and
			// re-arm with the new value. We don't perform a heartbeat
			// here — the kick is purely a re-cadence signal.
			if !heartbeatTimer.Stop() {
				select {
				case <-heartbeatTimer.C:
				default:
				}
			}
			armHeartbeat()
			continue

		case <-heartbeatTimer.C:
			// Re-arm for the next tick before processing so a slow
			// heartbeat call doesn't compound onto subsequent intervals.
			armHeartbeat()
			hbResp, err := c.httpHeartbeat(ctx)
			if err != nil {
				c.logger.Warn("HTTP heartbeat failed",
					slog.String("event", "heartbeat_failed"),
					slog.String("error", err.Error()),
				)
				continue
			}

			if hbResp.DesiredVer > c.reportedVer.Load() {
				if hbResp.Desired != nil {
					c.recordKeyVersions(hbResp.Desired)
					c.desiredVer.Store(hbResp.DesiredVer)
					c.applyConfig(hbResp.Desired, hbResp.DesiredVer)
				} else {
					c.logger.Info("Config version mismatch, pulling config",
						slog.String("event", "config_pull"),
						slog.Int64("desired_ver", hbResp.DesiredVer),
						slog.Int64("reported_ver", c.reportedVer.Load()),
					)
					pullResp, err := c.httpConfigPull(ctx)
					if err != nil {
						c.logger.Warn("Config pull failed",
							slog.String("event", "config_pull_failed"),
							slog.String("error", err.Error()),
						)
						continue
					}
					c.recordKeyVersions(pullResp.Configs)
					c.desiredVer.Store(pullResp.DesiredVer)
					c.applyConfig(pullResp.Configs, pullResp.DesiredVer)
				}
			}

		case <-time.After(wsRetryDelay):
			c.logger.Debug("Attempting WebSocket recovery",
				slog.String("event", "ws_recovery_attempt"),
				slog.Int("recovery_failures", recoveryFailures),
				slog.Duration("backoff", wsRetryDelay),
			)
			if err := dial(ctx); err == nil {
				c.logger.Info("WebSocket recovered, switching back from HTTP fallback",
					slog.String("event", "ws_recovered"),
				)
				c.wsConsecutiveFailures.Store(0)
				c.setMode(ModeWSConnected)
				c.promMetrics.wsConnections.WithLabelValues("success").Inc()
				c.promMetrics.wsConnected.Set(1)

				if c.onReconnect != nil {
					c.onReconnect()
				}

				c.runWSSession(ctx)
				c.promMetrics.wsConnected.Set(0)
				if c.onDisconnect != nil {
					c.onDisconnect()
				}
				return
			}
			recoveryFailures++
			// connectWS leaves mode ws_connecting on failure; restore HTTP fallback
			// so heartbeat-driven applyConfig can send shadow_report via HTTP.
			c.setMode(ModeHTTPFallback)
		}
	}
}

// --- URL Helpers ---

// deriveHTTPURL converts a WebSocket URL to an HTTP URL.
// "wss://host:port/ws" -> "https://host:port"
// "ws://host:port/ws" -> "http://host:port"
func deriveHTTPURL(wsURL string) string {
	u := wsURL

	if idx := indexAfterSchemeHost(u); idx > 0 {
		u = u[:idx]
	}

	if len(u) > 3 && u[:4] == "wss:" {
		u = "https:" + u[4:]
	} else if len(u) > 2 && u[:3] == "ws:" {
		u = "http:" + u[3:]
	}

	return u
}

// indexAfterSchemeHost finds the position of the path component after scheme://host:port.
func indexAfterSchemeHost(u string) int {
	schemeEnd := -1
	for i := range len(u) - 2 {
		if u[i] == ':' && u[i+1] == '/' && u[i+2] == '/' {
			schemeEnd = i + 3
			break
		}
	}
	if schemeEnd < 0 {
		return -1
	}
	for i := schemeEnd; i < len(u); i++ {
		if u[i] == '/' {
			return i
		}
	}
	return len(u)
}
