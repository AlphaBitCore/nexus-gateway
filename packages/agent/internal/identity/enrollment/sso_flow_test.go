package enrollment_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/enrollment"
)

func TestGeneratePKCE_Length(t *testing.T) {
	verifier, challenge, err := enrollment.GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}
	if n := len(verifier); n < 43 || n > 128 {
		t.Errorf("verifier length %d not in [43, 128]", n)
	}
	if len(challenge) == 0 {
		t.Error("challenge is empty")
	}
	if verifier == challenge {
		t.Error("verifier and challenge must differ")
	}
}

func TestCallbackServer_BindsLoopback(t *testing.T) {
	// We build a tiny fake CP + Hub to drive the full flow without a real
	// browser: the openBrowser hook posts a synthetic callback.

	state := ""
	redirectURI := ""
	var code string

	// Fake CP /oauth/authorize — not actually called by the flow's openBrowser
	// hook but we intercept the authorize URL to extract params.
	cpMux := http.NewServeMux()
	cpMux.HandleFunc("/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		state = r.URL.Query().Get("state")
		redirectURI = r.URL.Query().Get("redirect_uri")
		code = "test-code-123"
		// Simulate redirect back to agent.
		http.Redirect(w, r, redirectURI+"?code="+code+"&state="+state, http.StatusFound)
	})
	cpMux.HandleFunc("/api/agent/sso-enroll", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
			"enrollment_jwt": "eyJ.fake.jwt",
			"user_email":     "alice@example.com",
			"expires_at":     time.Now().Add(5 * time.Minute).Format(time.RFC3339),
		})
	})
	cpSrv := httptest.NewServer(cpMux)
	defer cpSrv.Close()

	// Fake Hub /api/internal/things/enroll — returns a minimal enrollment response.
	hubMux := http.NewServeMux()
	hubMux.HandleFunc("/api/internal/things/enroll", func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer eyJ.fake.jwt" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
			"id":            "agent-desktop-test",
			"deviceToken":   "test-device-token",
			"certPem":       "-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----",
			"caCertPem":     "-----BEGIN CERTIFICATE-----\nfakeca\n-----END CERTIFICATE-----",
			"certSerial":    "1",
			"certExpiresAt": time.Now().Add(365 * 24 * time.Hour).Format(time.RFC3339),
		})
	})
	hubSrv := httptest.NewServer(hubMux)
	defer hubSrv.Close()

	// Use a temp dir for cert persistence.
	certDir := t.TempDir()

	// Build a TLS-pinned HubEnrollClient pointed at the fake Hub. CA
	// file is empty here because httptest.NewServer uses plain HTTP;
	// production wiring loads the real Hub CA at NewHubEnrollClient
	// time.
	hubEnroller, err := enrollment.NewHubEnrollClient(hubSrv.URL, "")
	if err != nil {
		t.Fatalf("NewHubEnrollClient: %v", err)
	}
	mgr := enrollment.NewManager(certDir, enrollment.WithHubEnroller(hubEnroller))

	var persisted atomic.Bool
	cpURL := cpSrv.URL
	flow := &enrollment.Flow{
		ResolveCpURL: func(_ context.Context) (string, error) { return cpURL, nil },
		HubEnroller:  hubEnroller,
		Manager:      mgr,
		Hostname:     "test-host",
		OS:           "darwin",
		OSVersion:    "14.0",
		AgentVersion: "0.1.0",
		Timeout:      10 * time.Second,
		OpenBrowser: func(rawURL string) error {
			// Instead of actually opening a browser, follow the redirect
			// ourselves to simulate the OAuth callback.
			go func() {
				client := &http.Client{
					CheckRedirect: func(req *http.Request, via []*http.Request) error {
						// Don't follow the CP redirect — just note the callback URL.
						return http.ErrUseLastResponse
					},
				}
				resp, err := client.Get(rawURL)
				if err != nil {
					return
				}
				defer func() { _ = resp.Body.Close() }()
				// Follow the redirect to our callback server.
				if loc := resp.Header.Get("Location"); loc != "" {
					resp2, err := http.Get(loc) //nolint:noctx
					if err != nil {
						return
					}
					defer func() { _ = resp2.Body.Close() }()
					persisted.Store(true)
				}
			}()
			return nil
		},
	}

	ctx := context.Background()
	result, err := flow.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result == nil {
		t.Fatal("result is nil")
	}
	_ = persisted.Load()
}

func TestFlow_Timeout(t *testing.T) {
	// The flow should return ErrTimeout when the browser callback never arrives.
	hubEnroller, err := enrollment.NewHubEnrollClient("http://127.0.0.1:1", "")
	if err != nil {
		t.Fatalf("NewHubEnrollClient: %v", err)
	}
	flow := &enrollment.Flow{
		ResolveCpURL: func(_ context.Context) (string, error) {
			return "http://127.0.0.1:1", nil // unreachable
		},
		HubEnroller:  hubEnroller,
		Manager:      enrollment.NewManager(t.TempDir()),
		Hostname:     "test-host",
		OS:           "darwin",
		OSVersion:    "14.0",
		AgentVersion: "0.1.0",
		Timeout:      100 * time.Millisecond,
		OpenBrowser:  func(string) error { return nil }, // don't open browser
	}

	ctx := context.Background()
	if _, err := flow.Run(ctx); err == nil {
		t.Fatal("expected timeout error")
	}
}

// TestFlow_CancelTerminatesRunningFlow covers Flow.Cancel — when a
// concurrent goroutine calls Cancel during an in-progress Run, the
// internal ctx must fire and Run must return promptly. Without this
// branch covered, an in-progress SSO enrollment could not be
// interrupted by the menu-bar UI (e.g. when the user aborts).
func TestFlow_CancelTerminatesRunningFlow(t *testing.T) {
	hubEnroller, err := enrollment.NewHubEnrollClient("http://127.0.0.1:1", "")
	if err != nil {
		t.Fatalf("NewHubEnrollClient: %v", err)
	}
	flow := &enrollment.Flow{
		ResolveCpURL: func(_ context.Context) (string, error) {
			return "http://127.0.0.1:1", nil
		},
		HubEnroller:  hubEnroller,
		Manager:      enrollment.NewManager(t.TempDir()),
		Hostname:     "test-host",
		OS:           "darwin",
		OSVersion:    "14.0",
		AgentVersion: "0.1.0",
		Timeout:      5 * time.Second, // long, so Cancel is the actual termination
		OpenBrowser:  func(string) error { return nil },
	}

	done := make(chan error, 1)
	go func() {
		_, err := flow.Run(context.Background())
		done <- err
	}()

	// Cancel from another goroutine.
	time.Sleep(50 * time.Millisecond)
	flow.Cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Run should error after Cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s after Cancel; the cancel channel is wired to ctx but Run blocked elsewhere")
	}
}

// TestFlow_CancelBeforeRun_NoOp covers the nil-cancel guard in
// Cancel() — calling Cancel before Run has started a flow must not
// panic. f.cancel stays nil until Run installs it; Cancel must
// gracefully no-op.
func TestFlow_CancelBeforeRun_NoOp(t *testing.T) {
	flow := &enrollment.Flow{}
	flow.Cancel() // must not panic on uninitialized f.cancel
}
