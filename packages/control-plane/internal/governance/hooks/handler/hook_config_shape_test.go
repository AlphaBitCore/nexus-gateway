package hooks

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
)

// TestCreateHookConfig_NonObjectConfig_Returns400 is the regression test for
// the fail-open restart bug: a hook config blob that is not a JSON object
// used to be stored verbatim; every gateway / compliance-proxy config reload
// then failed (freezing propagation at last-good), and a service restart
// started the pipeline with an EMPTY hook config — a silent compliance
// bypass. The write boundary must reject the shape instead.
func TestCreateHookConfig_NonObjectConfig_Returns400(t *testing.T) {
	cases := []struct {
		name   string
		config string
	}{
		{"array", `["not-an-object"]`},
		{"scalar-number", `42`},
		{"scalar-string", `"x"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandler(nil, nil, nil, audit.NewWriter(nil, "", slog.Default()))
			body := `{"name":"x","type":"webhook","implementationId":"noop","config":` + tc.config + `}`
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			c, _ := echoCtx(req, rec, "u1")
			_ = h.CreateHookConfig(c)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s; want 400", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "config must be a JSON object") {
				t.Errorf("missing shape message; body: %s", rec.Body.String())
			}
		})
	}
}

// TestUpdateHookConfig_NonObjectConfig_Returns400 covers the update wiring,
// which re-marshals the bound `any` config before the shape check.
func TestUpdateHookConfig_NonObjectConfig_Returns400(t *testing.T) {
	mock, meta := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT .* FROM "HookConfig" WHERE id`).
		WithArgs("hc-1").
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(makeHookConfigRow(now)...))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"config":[1,2]}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("hc-1")
	_ = h.UpdateHookConfig(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s; want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "config must be a JSON object") {
		t.Errorf("missing shape message; body: %s", rec.Body.String())
	}
}
