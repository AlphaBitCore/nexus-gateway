// Package ssoenroll orchestrates SSO-based device enrollment for the agent.
// It drives a PKCE OAuth flow against the Control Plane, exchanges the
// authorization code for an enrollment JWT, and registers the device with
// the Hub.
package enrollment

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"

	metricsplatform "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/platform"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// The following package-level variables exist solely as test seams so unit
// tests can exercise the crypto/rand-failure arms in generateNonce and
// generateSSODeviceIdentity (ecdsa.GenerateKey / x509.CreateCertificate /
// MarshalECPrivateKey), the net.Listen-failure arm in newCallbackServer,
// and the per-OS dispatch + exec.Command.Start arms in openBrowser.
// Production never reassigns them. Mirrors the established pattern in
// packages/agent/internal/identity/enrollment/enroll.go (randReader),
// packages/agent/internal/network/tls/engine.go (tlsRandReader), and
// packages/agent/internal/identity/secretstore/fallback.go (osFile +
// createTempFn).
var (
	ssoRandReader       io.Reader = rand.Reader
	ssoMarshalECPrivKey           = x509.MarshalECPrivateKey
	ssoNetListen                  = net.Listen
	ssoRuntimeGOOS                = runtime.GOOS
	ssoExecCommandStart           = func(name string, args ...string) error {
		// Defense-in-depth: refuse to spawn a real browser process when the
		// binary is a test binary. Any test that reaches this seam without
		// installing a stub (e.g. via enrollment.SetExecCommandStart) would
		// otherwise pop an OS browser tab to a transient httptest URL on a
		// developer workstation. testing.Testing() is true only in binaries
		// built by `go test`; production binaries return false and execute
		// the real shell-out.
		if testing.Testing() {
			return fmt.Errorf("ssoenroll: openBrowser called from a test binary without a stub; call enrollment.SetExecCommandStart to inject one")
		}
		return exec.Command(name, args...).Start() //nolint:noctx // fire-and-forget browser launch, no ctx to bind to
	}
)

// ErrCancelled is returned when the flow is cancelled via Cancel.
var ErrCancelled = errors.New("ssoenroll: cancelled")

// ErrTimeout is returned when the OAuth browser callback does not arrive in time.
var ErrTimeout = errors.New("ssoenroll: timed out waiting for browser callback")

const (
	// defaultTimeout caps the total wall-clock budget for one SSO
	// enrollment attempt. The dominant cost is the user finishing
	// browser OAuth (popup → IdP login → optional MFA → consent →
	// callback) which empirically can take many minutes if the user
	// gets distracted or the IdP is slow. 30 minutes is generous
	// enough that we never bail before the user does.
	defaultTimeout = 30 * time.Minute
	oauthClientID  = "agent-desktop"
)

// Result is returned by Run on successful enrollment.
type Result struct {
	Email   string
	ThingID string
}

// Flow drives a single SSO enrollment attempt.
type Flow struct {
	// ResolveCpURL discovers the Control Plane base URL at flow-start
	// time. Required. Typically wired to bootstrap.Client.Get so the
	// URL comes from Hub's /api/public/agent-bootstrap endpoint with a
	// per-agent YAML override as fallback.
	ResolveCpURL func(ctx context.Context) (string, error)
	// HubEnroller is reused from the Manager so the Bearer-JWT enrollment
	// hits the same TLS-pinned HTTP client as the legacy X-Enrollment-Token
	// path. Required.
	HubEnroller HubEnroller
	// Manager is used to persist enrollment artifacts to disk after Hub signs the cert.
	Manager *Manager
	// Hostname, OS, OSVersion, AgentVersion identify this device in the enrollment request.
	Hostname     string
	OS           string
	OSVersion    string
	AgentVersion string

	// CpHTTPClient is the HTTP client used to call POST
	// /api/agent/sso-enroll on the Control Plane. Should be a TLS-pinned
	// client. Nil falls back to http.DefaultClient (acceptable only for
	// local dev where CP is reached over plain HTTP).
	CpHTTPClient *http.Client

	// OpenBrowser opens a URL in the default browser. When nil the default
	// platform-specific command is used.
	OpenBrowser func(rawURL string) error

	// Timeout is how long to wait for the OAuth browser callback.
	// 0 means defaultTimeout (30 minutes).
	Timeout time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc

	// cpURL is the resolved Control Plane base URL, populated at the
	// start of Run and used by ssoEnroll / buildAuthorizeURL during
	// that run.
	cpURL string
}

// Run executes the full SSO enrollment flow. It blocks until enrollment
// succeeds, fails, or is cancelled via Cancel.
func (f *Flow) Run(ctx context.Context) (*Result, error) {
	timeout := f.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	f.mu.Lock()
	f.cancel = cancel
	f.mu.Unlock()
	defer cancel()

	// 0. Resolve the Control Plane URL via Hub bootstrap (or YAML
	// override). Done first so any operator misconfiguration surfaces
	// before we open a browser.
	if f.ResolveCpURL == nil {
		return nil, fmt.Errorf("ssoenroll: ResolveCpURL not configured")
	}
	cpURL, err := f.ResolveCpURL(ctx)
	if err != nil {
		return nil, fmt.Errorf("ssoenroll: resolve CP URL: %w", err)
	}
	f.cpURL = cpURL

	// 1. Generate PKCE verifier + challenge.
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		return nil, fmt.Errorf("ssoenroll: PKCE: %w", err)
	}

	// 2. Generate state nonce (same entropy pool as PKCE verifier).
	state, err := generateNonce()
	if err != nil {
		return nil, fmt.Errorf("ssoenroll: state nonce: %w", err)
	}

	// 3. Start ephemeral callback server on 127.0.0.1:0.
	srv, err := newCallbackServer()
	if err != nil {
		return nil, fmt.Errorf("ssoenroll: start callback server: %w", err)
	}
	defer srv.Close()

	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", srv.Port())

	// 4. Build OAuth authorize URL.
	authorizeURL := f.buildAuthorizeURL(redirectURI, challenge, state)

	// 5. Open browser.
	openFn := f.OpenBrowser
	if openFn == nil {
		openFn = openBrowser
	}
	if err := openFn(authorizeURL); err != nil {
		slog.Warn("ssoenroll: could not open browser automatically",
			"url", authorizeURL, "error", err)
		// Non-fatal: user can paste the URL manually.
	}

	// 6. Wait for the OAuth callback.
	code, gotState, err := srv.Wait(ctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, ErrTimeout
		}
		return nil, fmt.Errorf("ssoenroll: callback: %w", err)
	}

	// 7. Validate state to prevent CSRF.
	if gotState != state {
		return nil, fmt.Errorf("ssoenroll: state mismatch (possible CSRF)")
	}

	// 8. Exchange code for enrollment JWT via CP.
	enrollJWT, userEmail, err := f.ssoEnroll(ctx, code, verifier, redirectURI)
	if err != nil {
		return nil, fmt.Errorf("ssoenroll: sso-enroll: %w", err)
	}

	// 9. Generate local keypair + self-signed device identity cert.
	keyPEM, certPEM, err := generateSSODeviceIdentity(f.Hostname)
	if err != nil {
		return nil, fmt.Errorf("ssoenroll: generate device identity: %w", err)
	}

	// 10. Call Hub with Bearer JWT to enroll the device. Routed through
	// the Manager's existing HubEnroller so the TLS-pinned client (CA
	// loaded by NewHubEnrollClient) is reused — neither code path
	// silently drops the CA pin.
	if f.HubEnroller == nil {
		return nil, fmt.Errorf("ssoenroll: HubEnroller not configured")
	}
	// Generate Ed25519 attestation CSR for traffic attestation.
	// Fail-open — empty CSR string when keygen fails; Hub treats absence
	// as "agent not requesting attestation yet" and enrolls normally.
	attestCsrPEM, attestKeyPEM := generateAttestationKeyMaterial(f.Hostname)

	hubResp, err := f.HubEnroller.EnrollWithJWT(ctx, enrollJWT, HubEnrollRequest{
		// ThingType is mandatory — Hub defaults empty to "agent" as of
		// the [[agent-desktop-type-mismatch-bug]] fix but we send it
		// explicitly to be robust against older Hub binaries.
		ThingType: "agent",
		Version:   f.AgentVersion,
		Hostname:  f.Hostname,
		OS:        f.OS,
		OSVersion: f.OSVersion,
		// Hardware-stable fingerprint lets Hub recognise the same
		// physical machine across re-enrollments (pkg reinstall, second
		// SSO account on the same Mac, ...) so it reuses the existing
		// thing_id instead of accumulating dead duplicates. Empty when
		// the host blocks ioreg / machine-id; Hub falls back to mint-new
		// in that case.
		DeviceFingerprint: metricsplatform.ComputeDeviceFingerprint(),
		AttestationCsrPem: attestCsrPEM,
	})
	if err != nil {
		return nil, fmt.Errorf("ssoenroll: hub enroll: %w", err)
	}

	// 11. Persist artifacts to disk (atomic; leaves existing certs unchanged on failure).
	if err := f.Manager.PersistEnrollment(hubResp, keyPEM, certPEM, attestKeyPEM); err != nil {
		return nil, fmt.Errorf("ssoenroll: persist enrollment: %w", err)
	}
	// Best-effort: surface the signed-in identity to the menu bar
	// across restarts via a tiny on-disk file. A failure here is
	// non-fatal — the cert has already been persisted and the device
	// is functionally enrolled; the menu just won't show the user
	// email until the next successful sign-in.
	if err := f.Manager.PersistSSOEmail(userEmail); err != nil {
		slog.Warn("ssoenroll: persist sso email", "error", err)
	}

	return &Result{Email: userEmail, ThingID: hubResp.ID}, nil
}

// Cancel aborts an in-progress Run. Safe to call from any goroutine.
func (f *Flow) Cancel() {
	f.mu.Lock()
	cancel := f.cancel
	f.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (f *Flow) buildAuthorizeURL(redirectURI, challenge, state string) string {
	u, _ := url.Parse(f.cpURL + "/oauth/authorize")
	q := u.Query()
	q.Set("client_id", oauthClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURI)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	u.RawQuery = q.Encode()
	return u.String()
}

type ssoEnrollRequest struct {
	Code         string `json:"code"`
	CodeVerifier string `json:"code_verifier"`
	RedirectURI  string `json:"redirect_uri"`
}

type ssoEnrollResponse struct {
	EnrollmentJWT string `json:"enrollment_jwt"`
	UserEmail     string `json:"user_email"`
	ExpiresAt     string `json:"expires_at"`
}

func (f *Flow) ssoEnroll(ctx context.Context, code, verifier, redirectURI string) (jwt, email string, err error) {
	body, _ := json.Marshal(ssoEnrollRequest{
		Code:         code,
		CodeVerifier: verifier,
		RedirectURI:  redirectURI,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		f.cpURL+"/api/agent/sso-enroll", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")

	client := f.CpHTTPClient
	if client == nil {
		client = nexushttp.New(nexushttp.Config{})
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("CP returned %d: %s", resp.StatusCode, string(respBody))
	}

	var out ssoEnrollResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", "", fmt.Errorf("decode: %w", err)
	}
	return out.EnrollmentJWT, out.UserEmail, nil
}

// generateSSODeviceIdentity creates a new ECDSA P-256 keypair and a
// self-signed device identity certificate. Returns keyPEM (EC PRIVATE KEY)
// and certPEM (CERTIFICATE). The cert is the agent's local identity
// artifact; Hub does not verify it (auth is by device bearer token), so it
// is self-signed rather than CA-signed.
func generateSSODeviceIdentity(hostname string) (keyPEM, certPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), ssoRandReader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}

	serial, err := rand.Int(ssoRandReader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate cert serial: %w", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: fmt.Sprintf("device-%s", hostname)},
		NotBefore:    now,
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	certDER, err := x509.CreateCertificate(ssoRandReader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create device cert: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := ssoMarshalECPrivKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return keyPEM, certPEM, nil
}

func generateNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(ssoRandReader, b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

func openBrowser(rawURL string) error {
	var cmd string
	var args []string
	switch ssoRuntimeGOOS {
	case "darwin":
		cmd = "open"
		args = []string{rawURL}
	case "linux":
		cmd = "xdg-open"
		args = []string{rawURL}
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", rawURL}
	default:
		return fmt.Errorf("unsupported OS: %s", ssoRuntimeGOOS)
	}
	return ssoExecCommandStart(cmd, args...)
}
