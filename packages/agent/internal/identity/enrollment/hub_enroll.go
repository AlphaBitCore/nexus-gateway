package enrollment

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/relay"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// HubEnrollRequest is the body for POST /api/internal/things/enroll on Hub.
type HubEnrollRequest struct {
	ThingType string `json:"thingType,omitempty"`
	ThingID   string `json:"thingId,omitempty"`
	Version   string `json:"version"`
	CsrPEM    string `json:"csrPem"`
	Hostname  string `json:"hostname"`
	OS        string `json:"os"`
	OSVersion string `json:"osVersion"`
	// DeviceFingerprint is the hardware-stable 128-bit hash computed by
	// opsmetrics.ComputeDeviceFingerprint. When present, Hub uses it to
	// dedupe re-enrollments from the same physical host: if a prior agent
	// already enrolled with the same fingerprint, the existing thing_id
	// is reused instead of minting a fresh row. Empty fingerprint (e.g.
	// sandboxed runtime that can't read ioreg / machine-id) falls back
	// to the legacy "always create new thing_id" path.
	DeviceFingerprint string `json:"deviceFingerprint,omitempty"`
	// AttestationCsrPem is the Ed25519 CSR the agent generates alongside
	// the P-256 mTLS CSR for traffic attestation. Hub signs it via
	// agentca.SignAttestationCSR (Ed25519-only, no ClientAuth EKU) and
	// stores the public-key bytes in thing_agent.sysinfo so the
	// compliance-proxy can look them up at verify time. Empty when the
	// agent build predates attestation — Hub tolerates absence and the
	// agent runs without the header until the next re-enrollment.
	AttestationCsrPem string `json:"attestationCsrPem,omitempty"`
}

// HubEnrollResponse is the response from Hub enrollment.
type HubEnrollResponse struct {
	ID          string `json:"id"`
	DeviceToken string `json:"deviceToken"`
	CertPEM     string `json:"certPem"`
	CaCertPEM   string `json:"caCertPem"`
	CertSerial  string `json:"certSerial"`
	CertExpires string `json:"certExpiresAt"`
	// TrustLevel is the Hub-computed level (0–3) as of the moment
	// enrollment completed. The agent persists it locally so the menu
	// bar UI can surface the current level without a Hub round-trip;
	// it is refreshed implicitly on every reenroll/renewal.
	TrustLevel int `json:"trustLevel,omitempty"`
	// AttestationCertPem is the signed Ed25519 attestation cert Hub
	// returns when the request carries an AttestationCsrPem. Empty
	// when the agent didn't request attestation (legacy build) or
	// when Hub's CA signing failed; the agent persists the field only
	// when non-empty so the absence-of-key path remains a clean
	// "attestation not available yet" signal.
	AttestationCertPem string `json:"attestationCertPem,omitempty"`
}

// HubDeregisterRequest is the body for POST /api/internal/things/deregister.
type HubDeregisterRequest struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// HubEnroller abstracts Hub enrollment so the Manager can be tested without
// a real Hub server.
type HubEnroller interface {
	Enroll(ctx context.Context, token string, req HubEnrollRequest) (*HubEnrollResponse, error)
	EnrollWithJWT(ctx context.Context, enrollmentJWT string, req HubEnrollRequest) (*HubEnrollResponse, error)
	Deregister(ctx context.Context, deviceToken, thingID, reason string) error
}

// HubEnrollClient is a minimal HTTP client for the Hub enrollment and
// deregister endpoints.
type HubEnrollClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewHubEnrollClient creates a HubEnrollClient. BaseURL is the Hub HTTP URL
// (e.g. "https://hub.example.com") without a trailing slash. When caCertFile
// is non-empty, the PEM at that path is loaded as the sole trust root for TLS
// (CA pinning); an agent with no device cert yet relies on this bootstrap CA
// to prevent MITM of the X-Enrollment-Token, so read/parse failures of an
// explicitly configured CA are fatal (fail closed) rather than silently
// downgrading to system trust. When caCertFile is empty, the system trust
// store is used (acceptable when the Hub CA is already in the OS trust store,
// or for plain HTTP in local dev).
func NewHubEnrollClient(baseURL, caCertFile string) (*HubEnrollClient, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caCertFile != "" {
		pem, err := os.ReadFile(caCertFile)
		if err != nil {
			return nil, fmt.Errorf("hub enroll: read CA file %q: %w", caCertFile, err)
		}
		// See hubhttp/client.go for rationale — start from the system
		// pool and append the pinned CA so Let's Encrypt-issued Hub
		// certs (and other public-PKI endpoints) still verify.
		pool, sysErr := x509.SystemCertPool()
		if sysErr != nil || pool == nil {
			pool = x509.NewCertPool()
			slog.Warn("hub enroll: SystemCertPool unavailable, falling back to pinned-only pool",
				"error", sysErr)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("hub enroll: parse CA PEM at %q: no valid certificates", caCertFile)
		}
		tlsCfg.RootCAs = pool
		slog.Info("hub enroll: loaded CA for TLS pinning (atop system roots)", "path", caCertFile)
	}

	httpC := nexushttp.New(nexushttp.Config{
		Timeout:             30 * time.Second,
		Caller:              "agent-enroll",
		PropagateReqID:      true,
		MaxIdleConnsPerHost: 5,
		IdleConnTimeout:     90 * time.Second,
		H2ReadIdleTimeout:   30 * time.Second,
		ForceHTTP2:          nexushttp.On(),
	})
	if err := relay.WithTLSConfig(httpC, tlsCfg); err != nil {
		return nil, fmt.Errorf("hub enroll: install TLS config: %w", err)
	}

	return &HubEnrollClient{
		BaseURL:    baseURL,
		HTTPClient: httpC,
	}, nil
}

// Enroll posts the enrollment request authenticated by the legacy
// admin-issued X-Enrollment-Token header (mtls-only mode).
func (c *HubEnrollClient) Enroll(ctx context.Context, token string, req HubEnrollRequest) (*HubEnrollResponse, error) {
	return c.doEnroll(ctx, req, "X-Enrollment-Token", token)
}

// EnrollWithJWT posts the enrollment request authenticated by an
// SSO-issued enrollment JWT carried in the Authorization: Bearer
// header (enterprise-login mode). Shares the same TLS-pinned HTTP
// client as Enroll so neither code path silently drops the CA pin.
func (c *HubEnrollClient) EnrollWithJWT(ctx context.Context, enrollmentJWT string, req HubEnrollRequest) (*HubEnrollResponse, error) {
	return c.doEnroll(ctx, req, "Authorization", "Bearer "+enrollmentJWT)
}

func (c *HubEnrollClient) doEnroll(ctx context.Context, req HubEnrollRequest, authHeader, authValue string) (*HubEnrollResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal hub enroll request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/internal/things/enroll", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create hub enroll request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(authHeader, authValue)

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("hub enroll request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read hub enroll response: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("hub enrollment rejected (401): %s", string(respBody))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hub enrollment failed (%d): %s", resp.StatusCode, string(respBody))
	}

	var result HubEnrollResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode hub enroll response: %w", err)
	}
	return &result, nil
}

func (c *HubEnrollClient) Deregister(ctx context.Context, deviceToken, thingID, reason string) error {
	body, err := json.Marshal(HubDeregisterRequest{ID: thingID, Reason: reason})
	if err != nil {
		return fmt.Errorf("marshal hub deregister: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/internal/things/deregister", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create hub deregister request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+deviceToken)

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("hub deregister request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("hub deregister failed (%d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}
