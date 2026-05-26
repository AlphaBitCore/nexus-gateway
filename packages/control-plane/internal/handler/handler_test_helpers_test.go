package handler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// hubSpy captures Hub calls made by admin handlers during unit tests. It
// satisfies the HubNotifier interface so tests can wire it directly onto an
// AdminHandler without standing up a real Hub.
type hubSpy struct {
	mu                 sync.Mutex
	notifyCalls        []hub.ConfigChangeRequest
	invalidateCalls    []hubInvalidateCall
	enrollmentCalls    []hub.CreateEnrollmentTokenRequest
	forceResyncCalls   []string
	rotateCertCalls    []string
	notifyResponse     *hub.ConfigChangeResponse
	enrollmentResponse *hub.CreateEnrollmentTokenResponse
	notifyErr          error
	enrollmentErr      error
	forceResyncErr     error
	rotateCertErr      error
	baseURL            string
	token              string
	serviceMeta        *hub.ThingServiceMeta
	serviceMetaErr     error
}

// hubInvalidateCall captures the (thingType, configKey) pair passed to
// InvalidateConfig so assertions can verify fire-and-forget notifications.
type hubInvalidateCall struct {
	ThingType string
	ConfigKey string
}

// NotifyConfigChange records the request and returns the spy's pre-configured
// response (or a default success response when none is set).
func (s *hubSpy) NotifyConfigChange(_ context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifyCalls = append(s.notifyCalls, req)
	if s.notifyErr != nil {
		return nil, s.notifyErr
	}
	if s.notifyResponse != nil {
		return s.notifyResponse, nil
	}
	return &hub.ConfigChangeResponse{OK: true, Version: 1, ThingsNotified: 1, ThingsOnline: 1}, nil
}

// InvalidateConfig records the call (fire-and-forget, no error return).
func (s *hubSpy) InvalidateConfig(_ context.Context, thingType, configKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidateCalls = append(s.invalidateCalls, hubInvalidateCall{ThingType: thingType, ConfigKey: configKey})
}

// ForceResyncAll records the thingID passed to the force-resync helper so tests
// can assert the admin force-refresh handler called through to the Hub.
func (s *hubSpy) ForceResyncAll(_ context.Context, thingID string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forceResyncCalls = append(s.forceResyncCalls, thingID)
	return map[string]any{"ok": true}, s.forceResyncErr
}

// RotateAgentCert records the thingID passed to the rotate-cert helper so tests
// can assert the admin rotate-cert handler called through to the Hub.
func (s *hubSpy) RotateAgentCert(_ context.Context, thingID string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rotateCertCalls = append(s.rotateCertCalls, thingID)
	return map[string]any{"ok": true}, s.rotateCertErr
}

// ListDLQ is a stub satisfying the HubNotifier surface for the DLQ admin
// proxy. Tests that exercise the DLQ admin route inject their own client.
func (s *hubSpy) ListDLQ(_ context.Context, _, _, _ string) ([]byte, int, error) {
	return []byte(`{"rows":[]}`), http.StatusOK, nil
}

// RetryDLQ is a stub satisfying the HubNotifier surface for the DLQ admin
// proxy. Tests that exercise the DLQ admin route inject their own client.
func (s *hubSpy) RetryDLQ(_ context.Context, _ string) ([]byte, int, error) {
	return []byte(`{"ok":true}`), http.StatusOK, nil
}

// CreateEnrollmentToken records the request and returns the spy's pre-configured
// response (or a default token when none is set).
func (s *hubSpy) CreateEnrollmentToken(_ context.Context, req hub.CreateEnrollmentTokenRequest) (*hub.CreateEnrollmentTokenResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enrollmentCalls = append(s.enrollmentCalls, req)
	if s.enrollmentErr != nil {
		return nil, s.enrollmentErr
	}
	if s.enrollmentResponse != nil {
		return s.enrollmentResponse, nil
	}
	return &hub.CreateEnrollmentTokenResponse{Token: "test-token"}, nil
}

// BaseURL returns the configured stub base URL.
// GetThingRuntime is a no-op stub; tests that exercise the runtime
// introspection bridge override it via a typed wrapper if they need
// canned responses. Returns 503 to signal "Hub unavailable" so the CP
// handler maps it to a clean error.
func (s *hubSpy) GetThingRuntime(_ context.Context, _ string) ([]byte, int, error) {
	return nil, http.StatusServiceUnavailable, nil
}

// GetThingServiceMeta is a no-op stub returning a zero-value meta with empty
// managementURL. Tests that exercise setup relay endpoints override this by
// setting s.serviceMeta.
func (s *hubSpy) GetThingServiceMeta(_ context.Context, thingID string) (*hub.ThingServiceMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.serviceMeta != nil {
		return s.serviceMeta, s.serviceMetaErr
	}
	return &hub.ThingServiceMeta{ThingID: thingID, ManagementURL: ""}, nil
}

func (s *hubSpy) BaseURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.baseURL
}

// Token returns the configured stub service token.
func (s *hubSpy) Token() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token
}

// Last returns the most recent NotifyConfigChange request. Panics if none was
// recorded — tests should assert at least one call was made before reading.
func (s *hubSpy) Last() hub.ConfigChangeRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.notifyCalls) == 0 {
		return hub.ConfigChangeRequest{}
	}
	return s.notifyCalls[len(s.notifyCalls)-1]
}

// NotifyCalls returns a copy of all recorded NotifyConfigChange requests.
func (s *hubSpy) NotifyCalls() []hub.ConfigChangeRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]hub.ConfigChangeRequest, len(s.notifyCalls))
	copy(out, s.notifyCalls)
	return out
}

// silentLogger returns a logger that discards output so handler tests do not
// spam stderr when they exercise error paths.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// auditSpy captures MQ enqueues so tests can assert admin audit entries.
type auditSpy struct {
	mu    sync.Mutex
	calls [][]byte
}

func (a *auditSpy) Publish(context.Context, string, []byte) error { return nil }
func (a *auditSpy) Enqueue(_ context.Context, _ string, data []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	copied := make([]byte, len(data))
	copy(copied, data)
	a.calls = append(a.calls, copied)
	return nil
}
func (a *auditSpy) Close() error { return nil }

// newAdminHandlerWithHubSpy wires an AdminHandler with an in-memory Hub spy,
// an MQ audit spy, and a silent logger. Other dependencies (DB, IAM, ...) are
// left nil because the handlers this helper is used with must not touch them.
func newAdminHandlerWithHubSpy(t *testing.T) (*AdminHandler, *hubSpy, *auditSpy) {
	t.Helper()
	spy := &hubSpy{}
	aud := &auditSpy{}
	h := &AdminHandler{
		Hub:    spy,
		Logger: silentLogger(),
		Audit:  audit.NewWriter(aud, "nexus.event.admin-audit", silentLogger()),
	}
	return h, spy, aud
}

// echoContext builds an Echo context with the authenticated admin principal
// populated under the same key the real AdminAuth middleware uses. The
// principal's KeyID / KeyName are propagated into ConfigChangeRequest.ActorID /
// ActorName by the handlers.
func echoContext(req *http.Request, rec *httptest.ResponseRecorder, userName, userID string) echo.Context {
	e := echo.New()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, &auth.AdminAuth{
		KeyID:             userID,
		KeyName:           userName,
		AuthPrincipalType: "admin_user",
	})
	return c
}

// assertErrorEnvelope checks errJSON-shaped error responses from admin handlers.
func assertErrorEnvelope(t *testing.T, rec *httptest.ResponseRecorder, wantCode, wantType string) {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal error body: %v; raw=%s", err, rec.Body.String())
	}
	if body.Error.Code != wantCode {
		t.Errorf("error.code = %q want %q", body.Error.Code, wantCode)
	}
	if body.Error.Type != wantType {
		t.Errorf("error.type = %q want %q", body.Error.Type, wantType)
	}
}
