package handler

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/conn"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/identity"
)

// handler.go — HandleConnections nil-conns branch

// nilReturningConnManager returns literal nil from ActiveConnections
// so HandleConnections enters the `if conns == nil { conns = []conn.
// ConnInfo{} }` branch. mockConnManager already guards against nil
// internally, so we need a separate stub here.
type nilReturningConnManager struct{}

func (nilReturningConnManager) ActiveCount() int64                 { return 0 }
func (nilReturningConnManager) ActiveConnections() []conn.ConnInfo { return nil }

// TestHandleConnections_NilConns_ReturnsEmptyArray pins the nil-guard
// in HandleConnections — a backing store that returns nil must still
// produce a JSON `[]` so the admin UI does not render `null`.
func TestHandleConnections_NilConns_ReturnsEmptyArray(t *testing.T) {
	deps := RuntimeDeps{
		ConnManager: nilReturningConnManager{},
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/connections", nil)
	HandleConnections(deps).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"connections":[]`) {
		t.Errorf("nil-conns body missing empty array, got %q", body)
	}
	if !strings.Contains(body, `"total":0`) {
		t.Errorf("nil-conns body missing total:0, got %q", body)
	}
}

// shadow_apply.go — ApplyActiveExemptions json decode error

// exemptionStoreFn is a function-typed ExemptionRebuilder for tests.
type exemptionStoreFn func([]identity.ActiveExemption)

func (f exemptionStoreFn) Rebuild(e []identity.ActiveExemption) { f(e) }

// TestApplyActiveExemptions_InvalidJSON covers the
// `json.Unmarshal(state, &v)` error branch in shadow_apply.go.
// Malformed shadow state must return a wrapped error so the
// break-glass handler can surface 400 instead of silently applying an
// empty entry list.
func TestApplyActiveExemptions_InvalidJSON(t *testing.T) {
	called := false
	store := exemptionStoreFn(func(_ []identity.ActiveExemption) { called = true })
	err := ApplyActiveExemptions(store, []byte(`{not json}`))
	if err == nil {
		t.Fatal("expected error for malformed exemptions state")
	}
	if !strings.Contains(err.Error(), "exemptions") {
		t.Errorf("error not wrapped with 'exemptions': %v", err)
	}
	if called {
		t.Errorf("Rebuild was invoked despite decode error")
	}
}
