package routing

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

// Test helpers ported verbatim from handler/handler_test_helpers_test.go
// per R6 runbook §4.2 option 1; promoted to a handler/util/ subpackage
// when the 4th domain extraction's copy demands it.

// hubSpy captures Hub calls made by routing handlers during unit tests.
// invalidateErr drives the push-failure → HTTP 502 branch.
type hubSpy struct {
	mu              sync.Mutex
	invalidateCalls []hubInvalidateCall
	invalidateErr   error
	notifyCalls     []hub.ConfigChangeRequest
	notifyErr       error
	notifyResponse  *hub.ConfigChangeResponse
}

type hubInvalidateCall struct {
	ThingType string
	ConfigKey string
}

func (s *hubSpy) InvalidateConfigE(_ context.Context, thingType, configKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidateCalls = append(s.invalidateCalls, hubInvalidateCall{ThingType: thingType, ConfigKey: configKey})
	return s.invalidateErr
}

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

func (s *hubSpy) NotifyCalls() []hub.ConfigChangeRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]hub.ConfigChangeRequest, len(s.notifyCalls))
	copy(out, s.notifyCalls)
	return out
}

// auditSpy captures MQ enqueues so tests can assert audit entries.
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

func (a *auditSpy) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newAdminHandlerWithHubSpy wires a routing Handler with an in-memory
// Hub spy + audit spy + silent logger. Named newAdminHandlerWithHubSpy
// to keep ported tests verbatim — the receiver type is routing.Handler
// not the parent AdminHandler god-object.
func newAdminHandlerWithHubSpy(t *testing.T) (*Handler, *hubSpy, *auditSpy) {
	t.Helper()
	spy := &hubSpy{}
	aud := &auditSpy{}
	h := New(Deps{
		Hub:    spy,
		Logger: silentLogger(),
		Audit:  audit.NewWriter(aud, "nexus.event.admin-audit", silentLogger()),
	})
	return h, spy, aud
}

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
