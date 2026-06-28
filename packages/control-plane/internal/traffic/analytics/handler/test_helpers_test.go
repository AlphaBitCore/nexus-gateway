package analytics

import (
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/settings/store/metricsstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/analytics/analyticsstore"
)

// Test helpers shared across the analytics handler tests. Mirrors the
// pgxmock + httptest seam established by sibling handler/passthrough
// (passthrough_handler_test.go) and handler/virtualkey (test_helpers_test.go).

// newMockHandler constructs a *Handler with a pgxmock pool standing in for
// both direct-SQL sites (h.pool) and store-method SQL (via analyticsstore/metricsstore).
func newMockHandler(t *testing.T) (pgxmock.PgxPoolIface, *Handler) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	h := &Handler{
		analytics: analyticsstore.New(mock),
		metrics:   metricsstore.New(mock),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		pool:      mock,
	}
	return mock, h
}

// echoCtx returns a fresh Echo context wired with a request + recorder.
func echoCtx(method, target string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	r := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	return e.NewContext(r, rec), rec
}

// withParam stamps a path param on an existing Echo context as the router
// would after a successful match.
func withParam(c echo.Context, name, value string) echo.Context {
	c.SetParamNames(name)
	c.SetParamValues(value)
	return c
}

// rollupCols is the column set every rollup SELECT in store/metrics_rollup.go
// returns. Used to seed pgxmock row sets when faking QueryRollupCascade /
// QueryRollupAware / QueryRollup payloads through the store package.
var rollupCols = []string{
	"id", "bucketStart", "metricName", "dimensionKey",
	"subDimension", "value", "metadata", "updatedAt",
}

// noRowsErr returns pgx.ErrNoRows so GetWatermark treats the watermark as
// missing and the cascade collapses each segment to an empty [t,t) window.
func noRowsErr() error { return pgx.ErrNoRows }

// jsonBody parses the response body as a map for tabular assertions.
func jsonBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("unmarshal body: %v; raw=%s", err, rec.Body.String())
	}
	return m
}

// assertStatus is a small wrapper that fails fast with the body on mismatch.
func assertStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, want, rec.Body.String())
	}
}

// iamMWNoop returns an IAM middleware that always permits the request. Used
// by RegisterRoutes assertions where we only care about path/verb mounts.
func iamMWNoop(_ string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
}
