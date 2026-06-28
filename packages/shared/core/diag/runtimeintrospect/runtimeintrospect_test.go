package runtimeintrospect_test

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
)

func newRegistry(t *testing.T) *runtimeintrospect.Registry {
	t.Helper()
	return runtimeintrospect.New("test-service", "thing-1", "0.1.0")
}

func TestRegistry_Snapshot_OKAndError(t *testing.T) {
	r := newRegistry(t)

	r.Register(runtimeintrospect.SourceFunc{
		SourceName: "config.foo",
		Fn: func(_ context.Context) (any, error) {
			return map[string]any{"enabled": true}, nil
		},
	})
	r.Register(runtimeintrospect.SourceFunc{
		SourceName: "config.bar",
		Fn: func(_ context.Context) (any, error) {
			return nil, errors.New("boom")
		},
	})

	snap := r.Snapshot(context.Background())
	if snap.Meta.Service != "test-service" {
		t.Errorf("service mismatch: got %q", snap.Meta.Service)
	}
	if got := snap.Sources["config.foo"]; !got.OK || got.Error != "" {
		t.Errorf("foo expected OK, got %+v", got)
	}
	if got := snap.Sources["config.bar"]; got.OK || got.Error != "boom" {
		t.Errorf("bar expected error 'boom', got %+v", got)
	}
}

type panicSrc struct{}

func (panicSrc) Name() string                            { return "panic" }
func (panicSrc) Snapshot(_ context.Context) (any, error) { panic("intentional") }

func TestRegistry_Snapshot_PanicIsolated(t *testing.T) {
	r := newRegistry(t)
	r.Register(runtimeintrospect.SourceFunc{
		SourceName: "good",
		Fn:         func(_ context.Context) (any, error) { return "ok", nil },
	})
	r.Register(panicSrc{})

	snap := r.Snapshot(context.Background())
	if got := snap.Sources["good"]; !got.OK {
		t.Fatalf("good source should still serve, got %+v", got)
	}
	if got := snap.Sources["panic"]; got.OK || !strings.Contains(got.Error, "panic") {
		t.Fatalf("panic source should be marked as error containing 'panic', got %+v", got)
	}
}

func TestRegistry_RegisterNilOrEmpty(t *testing.T) {
	r := newRegistry(t)
	r.Register(nil)
	r.Register(runtimeintrospect.SourceFunc{
		SourceName: "",
		Fn:         func(_ context.Context) (any, error) { return nil, nil },
	})
	if got := r.Names(); len(got) != 0 {
		t.Errorf("expected zero sources, got %v", got)
	}
}

func TestRegistry_RegisterDedup(t *testing.T) {
	r := newRegistry(t)
	r.Register(runtimeintrospect.SourceFunc{
		SourceName: "x",
		Fn:         func(_ context.Context) (any, error) { return 1, nil },
	})
	r.Register(runtimeintrospect.SourceFunc{
		SourceName: "x",
		Fn:         func(_ context.Context) (any, error) { return 2, nil },
	})
	snap := r.Snapshot(context.Background())
	if got := snap.Sources["x"].Value; got != 2 {
		t.Errorf("expected last registration to win, got %v", got)
	}
}

func TestHandler_AuthOK(t *testing.T) {
	r := newRegistry(t)
	r.Register(runtimeintrospect.SourceFunc{
		SourceName: "x",
		Fn:         func(_ context.Context) (any, error) { return 42, nil },
	})

	h := r.Handler(runtimeintrospect.HandlerOptions{Token: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/debug/runtime", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body)
	}
	var resp runtimeintrospect.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Sources["x"].OK {
		t.Errorf("source x should be OK")
	}
	if resp.Meta.Service != "test-service" {
		t.Errorf("meta.service mismatch")
	}
}

func TestHandler_AuthMissing(t *testing.T) {
	r := newRegistry(t)
	h := r.Handler(runtimeintrospect.HandlerOptions{Token: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/debug/runtime", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestHandler_AuthWrong(t *testing.T) {
	r := newRegistry(t)
	h := r.Handler(runtimeintrospect.HandlerOptions{Token: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/debug/runtime", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestHandler_NoTokenConfigured(t *testing.T) {
	r := newRegistry(t)
	h := r.Handler(runtimeintrospect.HandlerOptions{Token: ""})
	req := httptest.NewRequest(http.MethodGet, "/debug/runtime", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 (disabled), got %d", rec.Code)
	}
}

func TestHandler_MethodNotGET(t *testing.T) {
	r := newRegistry(t)
	h := r.Handler(runtimeintrospect.HandlerOptions{Token: "secret"})
	req := httptest.NewRequest(http.MethodPost, "/debug/runtime", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}
