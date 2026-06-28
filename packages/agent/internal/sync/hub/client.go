// Package hubhttp is the typed mTLS HTTP client for the
// /api/internal/things/* endpoints on Nexus Hub. It replaces the legacy
// gateway.Client after the hub-centric refactor: all agent outbound HTTP
// (enrollment, audit upload, exemption upload, update check, cert renew,
// deregister) flows through this single client.
//
// The client is intentionally minimal: it owns an mTLS http.Client with
// optional CA pinning and wraps the transport with otelhttp so W3C
// traceparent headers are injected on every outbound call.
package hub

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/clienttls"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/catrust"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// maxResponseBytes is the per-response cap applied when the client decodes
// JSON payloads from Hub. 10 MiB is comfortably above any realistic Hub
// response (cert bundles, update manifests) and guards against an unbounded
// read from a compromised or misbehaving peer.
const maxResponseBytes = 10 << 20

// Config configures the Hub HTTP client.
type Config struct {
	// HubURL is the Hub HTTP base URL (e.g. "https://hub.example.com")
	// without a trailing slash.
	HubURL string

	// CertFile / KeyFile are the agent device cert + key used as the mTLS
	// client identity.
	CertFile string
	KeyFile  string

	// CACertFile is an optional PEM file containing the Hub's CA. When
	// non-empty, it is loaded as the sole trust root (CA pinning). When
	// empty, the system trust store is used.
	CACertFile string

	// Timeout is the per-request timeout (default 30s).
	Timeout time.Duration

	// MaxRetries is the number of retry attempts for transient failures
	// (default 2, so up to 3 total attempts).
	MaxRetries int

	// RetryDelay is the fixed delay between retries (default 1s).
	RetryDelay time.Duration

	// DeviceTokenFn returns the agent's device bearer token at request
	// time. When non-nil and the returned string is non-empty, every
	// request emits "Authorization: Bearer <token>" — required by Hub's
	// DeviceOrServiceAuth middleware on every /api/internal/things/*
	// route. Without it endpoints like /update-check return 401 with
	// {"error":"missing authorization header","code":"UNAUTHORIZED"}.
	// Callback (not value) so the client can be constructed before
	// enrollment completes and pick up the token once it lands without
	// rebuilding the client. nil callback / empty return is allowed
	// for tests / pre-enrollment paths but every prod caller MUST supply a working callback — without it,
	// software update checks return 401 (hubhttp has no token plumbed).
	DeviceTokenFn func() string

	// ThingIDFn returns the agent's thing identifier at request time.
	// When non-nil and non-empty, every request emits X-Thing-Id —
	// required by Hub's device-token auth path alongside the bearer.
	ThingIDFn func() string

	// DeviceCAFile is the on-disk path of the Nexus device CA cert
	// (e.g. /var/lib/nexus-agent/device-ca.pem on Linux). When non-empty,
	// the cert is loaded and excluded from the system root pool used for
	// Hub TLS verification. This prevents a compromised device CA from being
	// used to forge Hub server certificates. Optional: when empty the
	// system pool is used as-is (acceptable when CACertFile provides a pin
	// that replaces the system pool entirely, or when the device CA is not
	// installed in the system trust store).
	DeviceCAFile string
}

// AuditEvent mirrors the agent-side audit event structure sent to
// POST /api/internal/things/audit. JSON tags MUST match the canonical
// TrafficEventMessage wire format (packages/shared/transport/mq/messages.go) —
// the Hub AuditUpload handler forwards this body straight to NATS,
// and the traffic_event consumer unmarshals into TrafficEventMessage
// where any unmatched tag falls back to the zero value (empty cells
// on /traffic). The agent-only fields (dest_ip / dest_port / bytes_in /
// bytes_out / policy_rule_id / source_user) ride inside `details` —
// they aren't first-class columns on traffic_event.
type AuditEvent struct {
	ID            string    `json:"id"`
	TraceID       string    `json:"traceId,omitempty"`
	Timestamp     time.Time `json:"timestamp"`
	SourceIP      string    `json:"sourceIp,omitempty"`
	SourceProcess string    `json:"sourceProcess"`
	// Canonical first-class request-row columns.
	TargetHost            string   `json:"targetHost,omitempty"`
	Method                string   `json:"method,omitempty"`
	Path                  string   `json:"path,omitempty"`
	StatusCode            int      `json:"statusCode,omitempty"`
	LatencyMs             int      `json:"latencyMs,omitempty"`
	Action                string   `json:"action"`
	BumpStatus            string   `json:"bumpStatus,omitempty"`
	RequestHookDecision   string   `json:"requestHookDecision,omitempty"`
	RequestHookReason     string   `json:"requestHookReason,omitempty"`
	RequestHookReasonCode string   `json:"requestHookReasonCode,omitempty"`
	ComplianceTags        []string `json:"complianceTags,omitempty"`

	// Agent-only metadata that doesn't have a first-class column on
	// traffic_event. Marshalled as a nested object under details.* so
	// the detail drawer can render bytes-in / bytes-out / destination
	// socket / policy rule / OS user without schema churn.
	Details json.RawMessage `json:"details,omitempty"`

	// PayloadRequest / PayloadResponse carry captured bytes on the HTTP
	// fallback upload path. Encoded as base64 on the wire by the standard
	// []byte marshaller so the Hub endpoint receives the same JSON shape
	// as the WebSocket upload (auditEventToMap). Empty when capture is
	// disabled.
	PayloadRequest  []byte `json:"payloadRequest,omitempty"`
	PayloadResponse []byte `json:"payloadResponse,omitempty"`

	// RequestSpillRef / ResponseSpillRef point at an oversize body the
	// drain step uploaded to S3 via the Hub presign flow. When set, the
	// inline Payload* field for that direction is empty — Hub demuxes
	// inline-vs-spill on receipt. JSON tags match Hub's AgentAuditEvent.
	RequestSpillRef  *sharedaudit.SpillRef `json:"requestSpillRef,omitempty"`
	ResponseSpillRef *sharedaudit.SpillRef `json:"responseSpillRef,omitempty"`
}

// ExemptionUpload is the body of POST /api/internal/things/exemption. The
// Hub endpoint requires thingId in the JSON body (unlike the legacy
// gateway endpoint which inferred it from the mTLS cert alone).
type ExemptionUpload struct {
	ThingID   string    `json:"thingId"`
	Host      string    `json:"host"`
	Reason    string    `json:"reason"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// UpdateInfo is the response from GET /api/internal/things/update-check.
type UpdateInfo struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	// DownloadURL is the HTTPS URL from which the updater fetches the binary.
	DownloadURL string `json:"downloadUrl,omitempty"`
	// Signature is the Ed25519 manifest signature (base64 StdEncoding).
	// It signs sha256( Version + ":" + SHA256 + ":" + DownloadURL ) — binding
	// the version string, binary hash, and download URL into a single signed
	// tuple so an attacker cannot replay an old signed binary at a new version
	// string. Verified BEFORE downloading the binary (fail-fast gate).
	Signature string `json:"signature,omitempty"`
	// BinarySignature is the Ed25519 binary-content signature (base64 StdEncoding).
	// It signs sha256(binary_bytes) — defense-in-depth verification that the
	// downloaded file content is the exact binary the release pipeline produced.
	// Verified AFTER downloading the binary, after SHA256 check passes.
	BinarySignature string `json:"binarySignature,omitempty"`
	SHA256          string `json:"sha256,omitempty"`
	ReleaseNotes    string `json:"releaseNotes,omitempty"`
	ForceUpdate     bool   `json:"forceUpdate,omitempty"`
}

// RenewTokenResponse is the response from POST /api/internal/things/renew-token.
// Hub mints a fresh device token, overwrites the stored hash (invalidating the
// old token), and returns the new plaintext plus its expiry.
type RenewTokenResponse struct {
	DeviceToken          string `json:"deviceToken"`
	DeviceTokenExpiresAt string `json:"deviceTokenExpiresAt"`
}

// Client is the Hub HTTP client.
type Client struct {
	cfg        Config
	httpClient *http.Client
}

// NewClient builds an mTLS Hub client. CertFile/KeyFile are optional (for
// pre-enrollment calls the agent has no device cert yet); when both are
// non-empty they are loaded as the client identity. When CACertFile is
// non-empty but unreadable, NewClient returns an error to fail closed on
// explicit pinning requests.
func NewClient(cfg Config) (*Client, error) {
	if cfg.HubURL == "" {
		return nil, fmt.Errorf("hubhttp: HubURL is required")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	// Default each retry knob independently: callers that override only one
	// must not silently inherit a zero value for the other (a zero RetryDelay
	// would busy-loop between attempts).
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 2
	}
	if cfg.RetryDelay == 0 {
		cfg.RetryDelay = 1 * time.Second
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}

	if cfg.CACertFile != "" {
		pemBytes, err := os.ReadFile(cfg.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("hubhttp: read CA file %q: %w", cfg.CACertFile, err)
		}
		// Start from the system root pool and APPEND the operator's
		// pinned CA — never replace. Replacing strands every public-PKI
		// endpoint the agent talks to: hub.example.com (Let's
		// Encrypt → ISRG Root X1), the auto-updater, and any future
		// non-private-CA Hub URL. Empty cfg.CACertFile keeps RootCAs
		// nil which means "use system roots" — same outcome on prod
		// where Hub uses Let's Encrypt and no pin is needed.
		// (without this, update-check fails with 'x509:
		//  certificate signed by unknown authority' when the pool is rebuilt
		//  fresh and the system roots get dropped.)
		pool, sysErr := x509.SystemCertPool()
		if sysErr != nil || pool == nil {
			pool = x509.NewCertPool()
			slog.Warn("hubhttp: SystemCertPool unavailable, falling back to pinned-only pool",
				"error", sysErr)
		}
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("hubhttp: parse CA PEM at %q: no valid certificates", cfg.CACertFile)
		}
		tlsCfg.RootCAs = pool
		slog.Info("hubhttp: loaded CA for TLS pinning (atop system roots)", "path", cfg.CACertFile)
	} else if cfg.DeviceCAFile != "" {
		// No Hub CA pin configured — Hub uses a public-PKI cert (e.g. Let's
		// Encrypt). Build a system pool with the Nexus device CA excluded so
		// a compromised device CA cannot forge Hub server certs.
		deviceCAPEM, err := os.ReadFile(cfg.DeviceCAFile)
		if err != nil {
			// Fail-open: device CA file missing (not yet installed or already
			// removed). Use the plain system pool and log a warning.
			slog.Warn("hubhttp: device CA not readable; using unfiltered system pool for Hub TLS",
				"path", cfg.DeviceCAFile, "error", err)
		} else {
			deviceCACert, err := parseSingleCertPEM(deviceCAPEM)
			if err != nil {
				slog.Warn("hubhttp: device CA parse failed; using unfiltered system pool for Hub TLS",
					"path", cfg.DeviceCAFile, "error", err)
			} else {
				pool, filterErr := catrust.SystemPoolExcluding(deviceCACert)
				if filterErr != nil {
					slog.Warn("hubhttp: SystemPoolExcluding failed; using unfiltered system pool",
						"error", filterErr)
				} else {
					tlsCfg.RootCAs = pool
					slog.Info("hubhttp: upstream TLS pool excludes Nexus device CA", "deviceCA", cfg.DeviceCAFile)
				}
			}
		}
	}

	if cfg.CertFile != "" && cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("hubhttp: load mTLS cert: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	httpC := nexushttp.New(nexushttp.Config{
		Timeout:             cfg.Timeout,
		Caller:              "agent-hub",
		PropagateReqID:      true,
		MaxIdleConnsPerHost: 5,
		IdleConnTimeout:     90 * time.Second,
		H2ReadIdleTimeout:   30 * time.Second,
		ForceHTTP2:          nexushttp.On(),
	})
	if err := clienttls.WithTLSConfig(httpC, tlsCfg); err != nil {
		return nil, fmt.Errorf("hubhttp: install TLS config: %w", err)
	}
	// Wrap the transport with otelhttp so W3C traceparent is injected on
	// every outbound call. The inner *http.Transport keeps doing per-host
	// pooling and HTTP/2 multiplex.
	innerTransport, err := clienttls.UnderlyingTransport(httpC)
	if err != nil {
		return nil, fmt.Errorf("hubhttp: extract inner transport: %w", err)
	}
	httpC.Transport = otelhttp.NewTransport(innerTransport)

	return &Client{
		cfg:        cfg,
		httpClient: httpC,
	}, nil
}

// HTTPClient returns the underlying *http.Client so callers that need the
// same secure transport for arbitrary downloads (e.g. the updater) can
// reuse it.
func (c *Client) HTTPClient() *http.Client {
	return c.httpClient
}

// BaseURL returns the configured Hub base URL.
func (c *Client) BaseURL() string {
	return c.cfg.HubURL
}

func (c *Client) doWithRetry(ctx context.Context, method, path string, body []byte, headers map[string]string) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(c.cfg.RetryDelay):
			}
		}

		// Defensive: the caller may have cancelled between attempts even
		// before the inter-attempt wait above (e.g. on the first attempt).
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.cfg.HubURL+path, bodyReader)
		if err != nil {
			return nil, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		// Hub's DeviceOrServiceAuth middleware requires the Authorization
		// + X-Thing-Id pair on every /api/internal/things/* route. Inject
		// before the per-call headers so a caller that explicitly passes
		// an X-Thing-Id (e.g. UploadAudit with a different deviceID for
		// drain-by-other-thing-id) can still override.
		if c.cfg.DeviceTokenFn != nil {
			if tok := c.cfg.DeviceTokenFn(); tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
		}
		if c.cfg.ThingIDFn != nil {
			if id := c.cfg.ThingIDFn(); id != "" {
				req.Header.Set("X-Thing-Id", id)
			}
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		start := time.Now()
		resp, err := c.httpClient.Do(req)
		duration := time.Since(start)
		if err != nil {
			// If the error is caused by ctx cancellation, surface that
			// directly so callers can detect it with errors.Is(err,
			// context.Canceled) or context.DeadlineExceeded. Wrapping it in
			// "request failed after N retries" would hide the cause.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			slog.Debug("hub request failed",
				"method", method, "path", path,
				"attempt", attempt, "duration", duration, "error", err)
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 {
			slog.Warn("hub server error",
				"method", method, "path", path,
				"status", resp.StatusCode, "attempt", attempt, "duration", duration)
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("server error: %d", resp.StatusCode)
			continue
		}
		slog.Debug("hub request ok",
			"method", method, "path", path,
			"status", resp.StatusCode, "duration", duration)
		return resp, nil
	}
	return nil, fmt.Errorf("request failed after %d retries: %w", c.cfg.MaxRetries, lastErr)
}

// UploadAudit calls POST /api/internal/things/audit with the given
// thingId + events, returning the accepted count reported by Hub.
//
// X-Thing-Id is mandatory on the Hub side device-token auth path; the
// deviceID is also placed in the body for legacy clients but the header
// is what the request-auth middleware reads. Without it Hub responds
// 401 "X-Thing-Id header required for device token auth" — observed
// during shutdown-drain after a daemon upgrade where the running
// process tried to flush its queued audit events before exit.
func (c *Client) UploadAudit(ctx context.Context, deviceID string, events []AuditEvent) (int, error) {
	body := map[string]any{"thingId": deviceID, "events": events}
	buf, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("marshal audit: %w", err)
	}
	resp, err := c.doWithRetry(ctx, http.MethodPost, "/api/internal/things/audit", buf,
		map[string]string{"X-Thing-Id": deviceID})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return 0, fmt.Errorf("audit upload failed (%d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Accepted int `json:"accepted"`
	}
	reader := http.MaxBytesReader(nil, resp.Body, maxResponseBytes)
	if err := json.NewDecoder(reader).Decode(&result); err != nil {
		return 0, fmt.Errorf("decode audit response: %w", err)
	}
	return result.Accepted, nil
}

// UploadExemption calls POST /api/internal/things/exemption.
func (c *Client) UploadExemption(ctx context.Context, e ExemptionUpload) error {
	buf, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal exemption: %w", err)
	}
	resp, err := c.doWithRetry(ctx, http.MethodPost, "/api/internal/things/exemption", buf, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return fmt.Errorf("exemption upload failed (%d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// CheckUpdate calls GET /api/internal/things/update-check. The agent's
// current version is sent; the Hub compares against the pinned update
// target and returns Available=false when versions match.
//
// osName is accepted by the method signature for call-site clarity (the
// updater passes runtime.GOOS) but is not currently sent on the wire: the
// Hub update-target template is per-agent-type, not per-OS. The parameter
// is retained so a future per-OS pinning scheme can light it up without
// changing the updater.
func (c *Client) CheckUpdate(ctx context.Context, currentVersion, osName string) (UpdateInfo, error) {
	_ = osName // reserved for future per-OS pinning; see doc comment
	q := url.Values{}
	q.Set("currentVersion", currentVersion)
	path := "/api/internal/things/update-check?" + q.Encode()
	resp, err := c.doWithRetry(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return UpdateInfo{}, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return UpdateInfo{}, fmt.Errorf("update check failed (%d): %s", resp.StatusCode, string(respBody))
	}

	var result UpdateInfo
	body := http.MaxBytesReader(nil, resp.Body, maxResponseBytes)
	if err := json.NewDecoder(body).Decode(&result); err != nil {
		return UpdateInfo{}, fmt.Errorf("decode update: %w", err)
	}
	return result, nil
}

// RenewDeviceToken calls POST /api/internal/things/renew-token to rotate the
// device bearer token. It carries no body: the call authenticates with
// the current device token + X-Thing-Id injected by doWithRetry, and Hub
// resolves the identity from that token. Because doWithRetry reads the token
// fresh from disk on each call (DeviceTokenFn), follow-up HTTP calls
// automatically pick up the rotated token once RenewDeviceToken persists it.
func (c *Client) RenewDeviceToken(ctx context.Context) (*RenewTokenResponse, error) {
	resp, err := c.doWithRetry(ctx, http.MethodPost, "/api/internal/things/renew-token", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("renew device token: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return nil, fmt.Errorf("renew device token failed (%d): %s", resp.StatusCode, string(respBody))
	}

	var result RenewTokenResponse
	body := http.MaxBytesReader(nil, resp.Body, maxResponseBytes)
	if err := json.NewDecoder(body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode renew device token: %w", err)
	}
	return &result, nil
}

// parseSingleCertPEM decodes the first PEM block from pemBytes and parses it
// as an x509.Certificate. Used to load the device CA for pool filtering.
func parseSingleCertPEM(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no CERTIFICATE PEM block found")
	}
	return x509.ParseCertificate(block.Bytes)
}
