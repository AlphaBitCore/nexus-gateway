// Coverage for adapter_types.go (IsValidAdapterType) and handler.go helpers
// (New / errJSON / internalServerError / actorFromContext / parsePagination /
// incrementConfigVersion).
package providers

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/peer"
)

func TestIsValidAdapterType(t *testing.T) {
	// Every canonical entry must pass — drift between this list and the
	// AI Gateway Format enum would silently accept invalid provider
	// configs through the admin API.
	for _, v := range ValidAdapterTypes {
		if !IsValidAdapterType(v) {
			t.Errorf("ValidAdapterTypes contains %q but IsValidAdapterType returned false", v)
		}
	}
	// Negative + canonicalisation guards.
	cases := []string{
		"",                 // empty rejected
		"OPENAI",           // case-sensitive
		"openai ",          // trailing whitespace rejected
		" openai",          // leading whitespace rejected
		"openai-compat",    // non-canonical
		"unknown",          // never registered
		"anthropic-claude", // bait substring
	}
	for _, v := range cases {
		if IsValidAdapterType(v) {
			t.Errorf("IsValidAdapterType(%q) = true; want false", v)
		}
	}
}

// conflictForUniqueViolation: every provider-create unique index must map to
// its own machine code so the UI can point at the offending field. The
// regression this guards: a Credential_name_key collision used to surface as
// PROVIDER_NAME_EXISTS, so changing the provider name never cleared the error.
func TestConflictForUniqueViolation(t *testing.T) {
	tests := []struct {
		name        string
		constraint  string
		provider    string
		wantCode    string
		msgContains string
	}{
		{"provider name interpolated", "Provider_name_key", "deepseek", "PROVIDER_NAME_EXISTS", "'deepseek'"},
		{"provider name no name", "Provider_name_key", "", "PROVIDER_NAME_EXISTS", "that name"},
		{"path prefix", "Provider_pathPrefix_key", "deepseek", "PROVIDER_PATH_EXISTS", "path prefix"},
		{"credential name", "Credential_name_key", "deepseek", "CREDENTIAL_NAME_EXISTS", "credential"},
		{"model code", "Model_code_key", "deepseek", "MODEL_CODE_EXISTS", "code"},
		{"model natural key", "Model_providerId_providerModelId_key", "deepseek", "MODEL_ALREADY_REGISTERED", "providerModelId"},
		{"unknown constraint falls back", "Some_other_key", "deepseek", "CONFLICT", "unique value"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg, code := conflictForUniqueViolation(
				&pgconn.PgError{Code: "23505", ConstraintName: tc.constraint}, tc.provider)
			if code != tc.wantCode {
				t.Errorf("code = %q; want %q", code, tc.wantCode)
			}
			if !strings.Contains(msg, tc.msgContains) {
				t.Errorf("msg = %q; want contains %q", msg, tc.msgContains)
			}
		})
	}
}

// New + Deps wiring

func TestNew_AllFieldsThreaded(t *testing.T) {
	mock, db := newMockStore(t)
	_ = mock
	hub := &hubSpy{}
	aud := &auditSpy{}
	_, rdb := newMiniRedis(t)
	vault := newTestVault(t)
	multi := newTestMultiVault(t)
	proxy := ProxyConfig{
		AIGatewayBase:          peer.Static("https://aigw.example"),
		AIGatewayInternalToken: "tok",
	}
	h := New(Deps{
		Pool:       db.InternalPool(),
		Hub:        hub,
		Audit:      audit.NewWriter(aud, "q", silentLogger()),
		Logger:     silentLogger(),
		Vault:      vault,
		MultiVault: multi,
		Proxy:      proxy,
		Redis:      rdb,
	})
	if h.providers == nil || h.hub != hub || h.vault != vault || h.multiVault != multi ||
		h.redis != rdb || h.proxy.AIGatewayInternalToken != "tok" || h.audit == nil || h.logger == nil {
		t.Errorf("New did not thread all fields: %+v", h)
	}
	// The Hub-backed URL provider is threaded and resolves at call time.
	if base, err := h.aiGatewayBase(context.Background()); err != nil || base != "https://aigw.example" {
		t.Errorf("aiGatewayBase = %q, %v; want threaded provider value", base, err)
	}
}

// errJSON / internalServerError

func TestErrJSON_Shape(t *testing.T) {
	got := errJSON("oops", "validation_error", "field-x")
	env, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error envelope, got: %v", got)
	}
	if env["message"] != "oops" || env["type"] != "validation_error" || env["code"] != "field-x" {
		t.Errorf("bad envelope: %+v", env)
	}
}

func TestInternalServerError_StatusAndBody(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := internalServerError(c, "boom"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"boom"`) || !strings.Contains(rec.Body.String(), `"server_error"`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestActorFromContext_PresentAndAbsent(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-7")
	got := actorFromContext(c)
	if got.UserID != "u-7" || got.Name != "admin-u-7" {
		t.Errorf("actor = %+v; want UserID=u-7 Name=admin-u-7", got)
	}

	anonC := anonEchoCtx(req, rec)
	got = actorFromContext(anonC)
	if got.UserID != "" || got.Name != "" {
		t.Errorf("absent auth must return zero actor; got %+v", got)
	}
}

func TestParsePagination(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantLimit  int
		wantOffset int
	}{
		{"defaults", "", 50, 0},
		{"happy custom", "limit=10&offset=20", 10, 20},
		{"zero limit ignored → default", "limit=0", 50, 0},
		{"negative offset ignored → default", "offset=-3", 50, 0},
		{"non-int values ignored", "limit=abc&offset=xyz", 50, 0},
		{"limit clamp at 1000", "limit=5000", 1000, 0},
		{"offset accepted at 0 explicitly", "offset=0", 50, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			req := httptest.NewRequest(http.MethodGet, "/?"+tc.query, nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			pg := parsePagination(c)
			if pg.Limit != tc.wantLimit || pg.Offset != tc.wantOffset {
				t.Errorf("limit=%d offset=%d; want %d/%d", pg.Limit, pg.Offset, tc.wantLimit, tc.wantOffset)
			}
		})
	}
}

func TestIncrementConfigVersion_FreshKey_StartsAtOne(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WithArgs("agent.config.version").
		WillReturnRows(pgxmock.NewRows([]string{"value"}))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte("1"), "system").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	h.incrementConfigVersion(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestIncrementConfigVersion_ExistingValueIncrements(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.config.version").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte("7")))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte("8"), "system").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	h.incrementConfigVersion(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestIncrementConfigVersion_MalformedValueTreatedAsZero(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.config.version").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte("not-a-number")))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte("1"), "system").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	h.incrementConfigVersion(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestIncrementConfigVersion_SetErrorLoggedNotPropagated(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.config.version").
		WillReturnRows(pgxmock.NewRows([]string{"value"}))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte("1"), "system").
		WillReturnError(errors.New("disk full"))

	var buf bytes.Buffer
	h := New(Deps{
		Pool:   db.InternalPool(),
		Audit:  audit.NewWriter(nil, "", silentLogger()),
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
	})
	h.incrementConfigVersion(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
	if !strings.Contains(buf.String(), "increment agent config version") {
		t.Errorf("expected error log; got: %s", buf.String())
	}
}

// Ensure store import survives even after we delete its only direct use —
// silences the unused-import linter without weakening signal.
var _ = store.NewWithPgxPool
