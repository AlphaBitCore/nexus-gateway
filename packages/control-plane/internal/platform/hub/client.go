// Package hubclient calls Nexus Hub HTTP APIs from Control Plane, authenticated
// with the dedicated HUB_CONFIG_TOKEN — the config-write
// (/api/hub) + admin-alerts authority, separate from the fleet-wide
// INTERNAL_SERVICE_TOKEN that gates /api/internal/things.
package hub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// ErrNotConfigured is returned when the Hub base URL is not set.
var ErrNotConfigured = errors.New("hubclient: Nexus Hub base URL is not configured")

// Client calls Hub /api/hub + /api/v1/admin/alerts endpoints with Bearer
// HUB_CONFIG_TOKEN auth.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
	logger     *slog.Logger
	// ledger is the durable Category-B propagation backstop. Nil when
	// no DB is wired (dev/test) — every ledger call then degrades to a plain
	// fire-once push, preserving the pre-ledger behaviour.
	ledger *Ledger
}

// New returns a Hub API client. hubConfigToken must match Hub auth.hubConfigToken
// (env HUB_CONFIG_TOKEN, [MUST MATCH] CP ↔ Hub).
// If baseURL is empty, methods that call Hub return ErrNotConfigured (graceful degradation).
func New(baseURL, hubConfigToken string, hc *http.Client, logger *slog.Logger) *Client {
	if hc == nil {
		hc = nexushttp.New(nexushttp.Config{
			Timeout:        30 * time.Second,
			Caller:         "cp-hubclient",
			PropagateReqID: true,
		})
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:      hubConfigToken,
		httpClient: hc,
		logger:     logger,
	}
}

// GetThingRuntime calls GET /api/hub/things/:id/runtime and returns the
// raw response body + HTTP status. The body is opaque to CP — it
// contains Hub's introspection envelope (snapshot + meta) which the UI
// consumes directly.
func (c *Client) GetThingRuntime(ctx context.Context, thingID string) ([]byte, int, error) {
	if c.baseURL == "" {
		return nil, 0, ErrNotConfigured
	}
	if c.token == "" {
		return nil, 0, fmt.Errorf("hubclient: HUB_CONFIG_TOKEN is not set")
	}

	url := fmt.Sprintf("%s/api/hub/things/%s/runtime", c.baseURL, thingID)
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	r.Header.Set("Authorization", "Bearer "+c.token)
	r.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(r)
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

// CreateEnrollmentTokenRequest mirrors Hub enrollment.GenerateRequest fields used by CP.
type CreateEnrollmentTokenRequest struct {
	ThingType string
	Label     string
	CreatedBy string
}

// CreateEnrollmentTokenResponse is the subset of Hub response needed for the admin API.
type CreateEnrollmentTokenResponse struct {
	Token     string
	ExpiresAt time.Time
}

// CreateEnrollmentToken calls POST /api/hub/enrollment/token.
func (c *Client) CreateEnrollmentToken(ctx context.Context, req CreateEnrollmentTokenRequest) (*CreateEnrollmentTokenResponse, error) {
	if c.baseURL == "" {
		return nil, ErrNotConfigured
	}
	if c.token == "" {
		return nil, fmt.Errorf("hubclient: HUB_CONFIG_TOKEN is not set")
	}

	payload := map[string]any{
		"thingType": firstNonEmpty(req.ThingType, "agent"),
		"label":     req.Label,
		"createdBy": req.CreatedBy,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("hubclient: encode body: %w", err)
	}

	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/hub/enrollment/token", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("hubclient: build request: %w", err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(r)
	if err != nil {
		return nil, fmt.Errorf("hubclient: enrollment token request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("hubclient: enrollment token failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("hubclient: decode response: %w", err)
	}
	if out.Token == "" {
		return nil, fmt.Errorf("hubclient: hub returned empty token")
	}
	return &CreateEnrollmentTokenResponse{Token: out.Token, ExpiresAt: out.ExpiresAt}, nil
}

// ConfigChangeRequest describes a config change to push via Hub.
type ConfigChangeRequest struct {
	ThingType string // "ai-gateway", "compliance-proxy", "agent", "control-plane"
	ConfigKey string // e.g. "routing", "hooks", "killswitch"
	State     any    // full state for Category A; nil for Category B (version bump only)
	Action    string // "update", "delete", "create" — defaults to "update"
	ActorID   string
	ActorName string
	SourceIP  string
}

// ConfigChangeResponse is the Hub response after a config update notification.
type ConfigChangeResponse struct {
	OK             bool  `json:"ok"`
	Version        int64 `json:"version"`
	ThingsNotified int   `json:"thingsNotified"`
	ThingsOnline   int   `json:"thingsOnline"`
}

// httpStatusError carries the Hub HTTP status code alongside the error message
// so NotifyConfigChange's retry loop can distinguish a retryable 5xx/network
// failure from a futile 4xx (a malformed body never becomes valid on retry).
type httpStatusError struct {
	status int
	msg    string
}

func (e *httpStatusError) Error() string { return e.msg }

// isRetryable reports whether a Hub config-change attempt should be re-tried.
// Network/transport errors (status 0) and 5xx server errors are transient and
// retryable; 4xx client errors (e.g. 422 Unprocessable Entity on a malformed
// body, 400 Bad Request) are deterministic — the identical request will fail
// identically, so re-trying only burns the backoff budget. Returns true for
// any non-HTTP error so a connection reset still gets its retries.
func isRetryable(err error) bool {
	var se *httpStatusError
	if errors.As(err, &se) {
		return se.status < 400 || se.status >= 500
	}
	return true
}

// NotifyConfigChange tells Hub about a config change so it can update desired state
// and push to connected Things. Retries up to 3 times with exponential backoff on
// transient failures (network errors / 5xx); a 4xx response is returned immediately
// without retry since the request body will not change between attempts.
// Returns the Hub response on success. On persistent failure the error is logged at
// warn level and returned — callers may choose to ignore it (fire-and-forget).
func (c *Client) NotifyConfigChange(ctx context.Context, req ConfigChangeRequest) (*ConfigChangeResponse, error) {
	if c.baseURL == "" {
		return nil, ErrNotConfigured
	}
	if c.token == "" {
		return nil, ErrNotConfigured
	}

	action := req.Action
	if action == "" {
		action = "update"
	}

	payload := map[string]any{
		"thingType": req.ThingType,
		"configKey": req.ConfigKey,
		"state":     req.State,
		"action":    action,
		"actorId":   req.ActorID,
		"actorName": req.ActorName,
		"sourceIp":  req.SourceIP,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("hubclient: encode config change body: %w", err)
	}

	backoffs := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}
	var lastErr error

	for attempt := 0; attempt <= len(backoffs); attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("hubclient: config change cancelled: %w", ctx.Err())
			case <-time.After(backoffs[attempt-1]):
			}
		}

		out, err := c.doConfigChange(ctx, body)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !isRetryable(err) {
			// A 4xx (e.g. 422 on a malformed body) will fail identically on
			// every retry — return now instead of exhausting the backoff
			// budget.
			c.logger.Warn("hubclient: config change rejected (non-retryable)",
				"attempt", attempt+1,
				"thingType", req.ThingType,
				"configKey", req.ConfigKey,
				"error", err,
			)
			return nil, fmt.Errorf("hubclient: config change rejected: %w", err)
		}
		c.logger.Warn("hubclient: config change attempt failed",
			"attempt", attempt+1,
			"thingType", req.ThingType,
			"configKey", req.ConfigKey,
			"error", err,
		)
	}

	return nil, fmt.Errorf("hubclient: config change failed after retries: %w", lastErr)
}

func (c *Client) doConfigChange(ctx context.Context, body []byte) (*ConfigChangeResponse, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/hub/config/update", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("hubclient: build request: %w", err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(r)
	if err != nil {
		return nil, fmt.Errorf("hubclient: config change request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, &httpStatusError{
			status: resp.StatusCode,
			msg:    fmt.Sprintf("hubclient: config change failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw))),
		}
	}

	var out ConfigChangeResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("hubclient: decode config change response: %w", err)
	}
	return &out, nil
}

// SetLedger attaches the durable Category-B propagation backstop. Called once
// during wiring after the DB pool exists; safe to leave unset (nil) in dev/test,
// in which case InvalidateConfigE behaves as a plain fire-once push.
func (c *Client) SetLedger(l *Ledger) { c.ledger = l }

// pushTypeBRaw is the single low-level Category-B push: bump the key's version
// so Things reload from the CP DB. ErrNotConfigured (Hub base URL unset in
// local/dev) maps to nil so dev writes still succeed without a Hub.
func (c *Client) pushTypeBRaw(ctx context.Context, thingType, configKey string) error {
	if _, err := c.NotifyConfigChange(ctx, ConfigChangeRequest{
		ThingType: thingType,
		ConfigKey: configKey,
	}); err != nil {
		if errors.Is(err, ErrNotConfigured) {
			return nil
		}
		return err
	}
	return nil
}

// InvalidateConfigE is the error-returning Category B invalidation: only the
// version needs to bump (Things reload from the CP DB). Security-sensitive
// Type-B writes — credentials, virtual keys, routing rules, quota policies/
// overrides, providers, models — call this so a failed push to Hub surfaces to
// the admin as HTTP 502 instead of being silently logged. Otherwise the data
// plane keeps serving stale config (e.g. a REVOKED credential / virtual key)
// while the UI reports success.
//
// When a ledger is attached the intended version is recorded before
// the push and the acked version stamped only on success, so a failed push is
// re-driven by ReconcilePending without an admin retry. ErrNotConfigured is
// treated as a no-op (returns nil) so dev writes still succeed without a Hub —
// and no intent is recorded, since there is nothing to reconcile.
func (c *Client) InvalidateConfigE(ctx context.Context, thingType, configKey string) error {
	if c.ledger == nil {
		return c.pushTypeBRaw(ctx, thingType, configKey)
	}
	seq, err := c.ledger.RecordIntent(ctx, thingType, configKey)
	if err != nil {
		// The ledger is a backstop, not the source of truth: if it is briefly
		// unavailable, still attempt the push so the live path is not blocked.
		c.logger.Warn("hubclient: propagation ledger record-intent failed",
			"thingType", thingType, "configKey", configKey, "error", err)
		return c.pushTypeBRaw(ctx, thingType, configKey)
	}
	if err := c.pushTypeBRaw(ctx, thingType, configKey); err != nil {
		// Leave acked < intended; ReconcilePending will re-drive it.
		return err
	}
	if err := c.ledger.MarkAcked(ctx, thingType, configKey, seq); err != nil {
		// The push succeeded; a missed ack only causes a harmless redundant
		// re-push on the next reconcile tick. Do not fail the admin request.
		c.logger.Warn("hubclient: propagation ledger mark-acked failed",
			"thingType", thingType, "configKey", configKey, "seq", seq, "error", err)
	}
	return nil
}

// ReconcilePending is the Category-B version-reconcile arm. It re-pushes
// every key whose last push was not confirmed (acked < intended) and stamps the
// acked sequence on success. Returns the number of keys healed. A nil ledger is
// a no-op. Satisfies configreconcile.PendingReconciler so the existing 60s
// reconcile loop drives it once per tick.
func (c *Client) ReconcilePending(ctx context.Context) (int, error) {
	if c.ledger == nil {
		return 0, nil
	}
	pending, err := c.ledger.ListPending(ctx)
	if err != nil {
		return 0, err
	}
	healed := 0
	for _, p := range pending {
		if err := c.pushTypeBRaw(ctx, p.ThingType, p.ConfigKey); err != nil {
			c.logger.Warn("hubclient: propagation reconcile re-push failed",
				"thingType", p.ThingType, "configKey", p.ConfigKey, "error", err)
			continue
		}
		if err := c.ledger.MarkAcked(ctx, p.ThingType, p.ConfigKey, p.IntendedSeq); err != nil {
			c.logger.Warn("hubclient: propagation reconcile mark-acked failed",
				"thingType", p.ThingType, "configKey", p.ConfigKey, "seq", p.IntendedSeq, "error", err)
			continue
		}
		healed++
	}
	return healed, nil
}

// InvalidateConfig is the fire-and-forget Category B wrapper for non-security
// config keys where a missed push self-heals (reconcile loop / next successful
// write). It delegates to InvalidateConfigE and logs — does not return — the
// error. Matches the old PubSub.PublishInvalidation() signature.
func (c *Client) InvalidateConfig(ctx context.Context, thingType, configKey string) {
	if err := c.InvalidateConfigE(ctx, thingType, configKey); err != nil {
		c.logger.Warn("hubclient: invalidate config fire-and-forget failed",
			"thingType", thingType,
			"configKey", configKey,
			"error", err,
		)
	}
}

// ActorIdentity carries the admin user identity forwarded to Hub via HTTP
// headers so Hub can record who performed the action in audit rows.
type ActorIdentity struct {
	ID    string // maps to X-Nexus-Actor-User-Id
	Email string // maps to X-Nexus-Actor-Email (omitted when empty)
}

// ThingServiceMeta holds management endpoint info for a service Thing.
type ThingServiceMeta struct {
	ThingID       string `json:"thingId"`
	ManagementURL string `json:"managementUrl"`
}

// GetThingServiceMeta calls GET /api/hub/things/:id/service-meta and returns
// the thing's management URL. Returns an error if the thing is not found or
// Hub is not configured.
func (c *Client) GetThingServiceMeta(ctx context.Context, thingID string) (*ThingServiceMeta, error) {
	if c.baseURL == "" {
		return nil, ErrNotConfigured
	}
	if c.token == "" {
		return nil, fmt.Errorf("hubclient: HUB_CONFIG_TOKEN is not set")
	}

	url := fmt.Sprintf("%s/api/hub/things/%s/service-meta", c.baseURL, thingID)
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+c.token)
	r.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(r)
	if err != nil {
		return nil, fmt.Errorf("hubclient: service-meta request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("hubclient: read service-meta body: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("hubclient: thing %s not found", thingID)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hubclient: service-meta failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out ThingServiceMeta
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("hubclient: decode service-meta: %w", err)
	}
	return &out, nil
}

// ForceResyncAll calls Hub's POST /api/hub/things/:id/resync with an empty
// body, which Hub interprets as "re-push every desired key for this thing"
// (RePushAllKeys). Returns the raw JSON response from Hub so callers can
// pass through to the admin client. Used by the admin Device Detail
// "Force config refresh" action.
func (c *Client) ForceResyncAll(ctx context.Context, thingID string) (map[string]any, error) {
	if c.baseURL == "" {
		return nil, ErrNotConfigured
	}
	if c.token == "" {
		return nil, fmt.Errorf("hubclient: HUB_CONFIG_TOKEN is not set")
	}
	url := fmt.Sprintf("%s/api/hub/things/%s/resync", c.baseURL, thingID)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{}`))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+c.token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(r)
	if err != nil {
		return nil, fmt.Errorf("hubclient: force-resync request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("hubclient: read force-resync body: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("hubclient: thing %s not found", thingID)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("hubclient: force-resync failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("hubclient: decode force-resync: %w", err)
	}
	return out, nil
}

// ListDLQ calls GET /api/hub/dlq with the supplied filters and returns the
// raw response body + HTTP status. The body is opaque to CP — it carries
// Hub's dlqListResponse envelope ({rows,total}) verbatim to the UI. Pass
// empty strings for filters that should not be applied.
func (c *Client) ListDLQ(ctx context.Context, subject, limit, offset string) ([]byte, int, error) {
	if c.baseURL == "" {
		return nil, 0, ErrNotConfigured
	}
	if c.token == "" {
		return nil, 0, fmt.Errorf("hubclient: HUB_CONFIG_TOKEN is not set")
	}
	q := ""
	add := func(k, v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		sep := "&"
		if q == "" {
			sep = "?"
		}
		q += sep + k + "=" + v
	}
	add("subject", subject)
	add("limit", limit)
	add("offset", offset)
	url := c.baseURL + "/api/hub/dlq" + q
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	r.Header.Set("Authorization", "Bearer "+c.token)
	r.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(r)
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

// RetryDLQ calls POST /api/hub/dlq/:id/retry. Returns the raw response
// body + HTTP status. CP wraps this with an AdminAuditLog write before
// forwarding the response to the caller.
func (c *Client) RetryDLQ(ctx context.Context, id string) ([]byte, int, error) {
	if c.baseURL == "" {
		return nil, 0, ErrNotConfigured
	}
	if c.token == "" {
		return nil, 0, fmt.Errorf("hubclient: HUB_CONFIG_TOKEN is not set")
	}
	url := fmt.Sprintf("%s/api/hub/dlq/%s/retry", c.baseURL, id)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return nil, 0, err
	}
	r.Header.Set("Authorization", "Bearer "+c.token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(r)
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

// BaseURL returns the Hub base URL (empty when Hub is not configured).
func (c *Client) BaseURL() string { return c.baseURL }

// Token returns the internal service token used for Hub auth.
func (c *Client) Token() string { return c.token }

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
