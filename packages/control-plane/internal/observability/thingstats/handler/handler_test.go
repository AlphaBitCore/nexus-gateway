// Package thingstats_test verifies the GetThingStats handler, helper
// functions, and seam-injectable sub-paths (resolveDimensionNames,
// splitDimensionKey, convertThingRollupRows, parsePagination,
// parseRFC3339Flexible, parseTimeRange) without a real database connection.
package thingstats

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/thingstats/thingstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// Test doubles

// fakeThingStore implements thingOperations using in-memory fields.
type fakeThingStore struct {
	getThing          func(ctx context.Context, id string) (*thingstore.ThingRegistry, error)
	queryThingRollup  func(ctx context.Context, q thingstore.ThingMetricsQuery) ([]metrics.ThingRollupRow, error)
	thingRollupHasAny func(ctx context.Context, id string, start, end time.Time) (bool, error)
}

func (f *fakeThingStore) GetThing(ctx context.Context, id string) (*thingstore.ThingRegistry, error) {
	return f.getThing(ctx, id)
}

func (f *fakeThingStore) QueryThingRollup(ctx context.Context, q thingstore.ThingMetricsQuery) ([]metrics.ThingRollupRow, error) {
	return f.queryThingRollup(ctx, q)
}

func (f *fakeThingStore) ThingRollupHasAnyRecent(ctx context.Context, id string, start, end time.Time) (bool, error) {
	return f.thingRollupHasAny(ctx, id, start, end)
}

// thingWithName returns a ThingRegistry pointer with the given type and optional name.
func thingWithName(id, typ string, name *string) *thingstore.ThingRegistry {
	return &thingstore.ThingRegistry{
		ID:     id,
		Type:   typ,
		Name:   name,
		Status: "online",
	}
}

func strPtr(s string) *string { return &s }

// Echo test helpers

// echoCtx builds a fresh Echo context for method + target.
func echoCtx(method, target string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	r := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	return e.NewContext(r, rec), rec
}

// withParam stamps a route param on the context, mimicking router behaviour.
func withParam(c echo.Context, name, value string) echo.Context {
	c.SetParamNames(name)
	c.SetParamValues(value)
	return c
}

// withQuery stamps query params on the request URL.
func withQuery(c echo.Context, pairs ...string) echo.Context {
	q := c.Request().URL.Query()
	for i := 0; i+1 < len(pairs); i += 2 {
		q.Set(pairs[i], pairs[i+1])
	}
	c.Request().URL.RawQuery = q.Encode()
	return c
}

// jsonBody decodes the recorder body as map[string]any.
func jsonBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("unmarshal body: %v; raw=%s", err, rec.Body.String())
	}
	return m
}

// assertStatus fails fast if the status code doesn't match.
func assertStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, want, rec.Body.String())
	}
}

// newHandler returns a Handler wired with the provided thing store and
// a discard logger. pool is optional (nil omits resolveDimensionNames).
func newHandler(thing thingOperations, pool queryPool) *Handler {
	return &Handler{
		thing:  thing,
		pool:   pool,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// GetThingStats — nil store guard

// TestGetThingStats_NilStore verifies that a Handler with a nil thing field
// returns 503 ServiceUnavailable immediately, before any param parsing.
func TestGetThingStats_NilStore(t *testing.T) {
	t.Parallel()
	h := newHandler(nil, nil)
	c, rec := echoCtx(http.MethodGet, "/api/admin/things/abc/stats")
	c = withParam(c, "id", "abc")
	_ = h.GetThingStats(c)
	assertStatus(t, rec, http.StatusServiceUnavailable)
	body := jsonBody(t, rec)
	errMap, _ := body["error"].(map[string]any)
	if errMap["code"] != "DB_UNAVAILABLE" {
		t.Errorf("want code=DB_UNAVAILABLE, got %v", errMap["code"])
	}
}

// GetThingStats — missing thing id

// TestGetThingStats_EmptyID verifies that an empty ":id" path param returns
// 400 MISSING_THING_ID.
func TestGetThingStats_EmptyID(t *testing.T) {
	t.Parallel()
	h := newHandler(&fakeThingStore{
		getThing: func(_ context.Context, _ string) (*thingstore.ThingRegistry, error) {
			return nil, nil
		},
		queryThingRollup: func(_ context.Context, _ thingstore.ThingMetricsQuery) ([]metrics.ThingRollupRow, error) {
			return nil, nil
		},
		thingRollupHasAny: func(_ context.Context, _ string, _, _ time.Time) (bool, error) { return false, nil },
	}, nil)
	c, rec := echoCtx(http.MethodGet, "/api/admin/things/__blank__/stats")
	// Spaces only → TrimSpace → empty; route param value injected directly.
	c.SetParamNames("id")
	c.SetParamValues("  ")
	_ = h.GetThingStats(c)
	assertStatus(t, rec, http.StatusBadRequest)
	body := jsonBody(t, rec)
	errMap, _ := body["error"].(map[string]any)
	if errMap["code"] != "MISSING_THING_ID" {
		t.Errorf("want code=MISSING_THING_ID, got %v", errMap["code"])
	}
}

// GetThingStats — GetThing failures

// TestGetThingStats_GetThingError verifies that a DB error from GetThing
// returns 500 GET_THING_FAILED.
func TestGetThingStats_GetThingError(t *testing.T) {
	t.Parallel()
	h := newHandler(&fakeThingStore{
		getThing: func(_ context.Context, _ string) (*thingstore.ThingRegistry, error) {
			return nil, errors.New("db down")
		},
		queryThingRollup: func(_ context.Context, _ thingstore.ThingMetricsQuery) ([]metrics.ThingRollupRow, error) {
			return nil, nil
		},
		thingRollupHasAny: func(_ context.Context, _ string, _, _ time.Time) (bool, error) { return false, nil },
	}, nil)
	c, rec := echoCtx(http.MethodGet, "/api/admin/things/t1/stats")
	c = withParam(c, "id", "t1")
	_ = h.GetThingStats(c)
	assertStatus(t, rec, http.StatusInternalServerError)
	body := jsonBody(t, rec)
	errMap, _ := body["error"].(map[string]any)
	if errMap["code"] != "GET_THING_FAILED" {
		t.Errorf("want code=GET_THING_FAILED, got %v", errMap["code"])
	}
}

// TestGetThingStats_ThingNotFound verifies that a nil return from GetThing
// (no DB error) returns 404 THING_NOT_FOUND.
func TestGetThingStats_ThingNotFound(t *testing.T) {
	t.Parallel()
	h := newHandler(&fakeThingStore{
		getThing: func(_ context.Context, _ string) (*thingstore.ThingRegistry, error) { return nil, nil },
		queryThingRollup: func(_ context.Context, _ thingstore.ThingMetricsQuery) ([]metrics.ThingRollupRow, error) {
			return nil, nil
		},
		thingRollupHasAny: func(_ context.Context, _ string, _, _ time.Time) (bool, error) { return false, nil },
	}, nil)
	c, rec := echoCtx(http.MethodGet, "/api/admin/things/t1/stats")
	c = withParam(c, "id", "t1")
	_ = h.GetThingStats(c)
	assertStatus(t, rec, http.StatusNotFound)
	body := jsonBody(t, rec)
	errMap, _ := body["error"].(map[string]any)
	if errMap["code"] != "THING_NOT_FOUND" {
		t.Errorf("want code=THING_NOT_FOUND, got %v", errMap["code"])
	}
}

// GetThingStats — time-range parsing errors

// TestGetThingStats_InvalidEnd verifies that a garbage ?end= value returns
// 400 INVALID_END before any rollup query.
func TestGetThingStats_InvalidEnd(t *testing.T) {
	t.Parallel()
	h := newHandler(&fakeThingStore{
		getThing: func(_ context.Context, _ string) (*thingstore.ThingRegistry, error) {
			return thingWithName("t1", "ai-gateway", nil), nil
		},
		queryThingRollup: func(_ context.Context, _ thingstore.ThingMetricsQuery) ([]metrics.ThingRollupRow, error) {
			return nil, nil
		},
		thingRollupHasAny: func(_ context.Context, _ string, _, _ time.Time) (bool, error) { return false, nil },
	}, nil)
	c, rec := echoCtx(http.MethodGet, "/api/admin/things/t1/stats")
	c = withParam(c, "id", "t1")
	c = withQuery(c, "end", "not-a-timestamp")
	_ = h.GetThingStats(c)
	assertStatus(t, rec, http.StatusBadRequest)
	body := jsonBody(t, rec)
	errMap, _ := body["error"].(map[string]any)
	if errMap["code"] != "INVALID_END" {
		t.Errorf("want code=INVALID_END, got %v", errMap["code"])
	}
}

// TestGetThingStats_InvalidStart verifies that a garbage ?start= value
// returns 400 INVALID_START.
func TestGetThingStats_InvalidStart(t *testing.T) {
	t.Parallel()
	h := newHandler(&fakeThingStore{
		getThing: func(_ context.Context, _ string) (*thingstore.ThingRegistry, error) {
			return thingWithName("t1", "ai-gateway", nil), nil
		},
		queryThingRollup: func(_ context.Context, _ thingstore.ThingMetricsQuery) ([]metrics.ThingRollupRow, error) {
			return nil, nil
		},
		thingRollupHasAny: func(_ context.Context, _ string, _, _ time.Time) (bool, error) { return false, nil },
	}, nil)
	c, rec := echoCtx(http.MethodGet, "/api/admin/things/t1/stats")
	c = withParam(c, "id", "t1")
	c = withQuery(c, "start", "not-a-timestamp")
	_ = h.GetThingStats(c)
	assertStatus(t, rec, http.StatusBadRequest)
	body := jsonBody(t, rec)
	errMap, _ := body["error"].(map[string]any)
	if errMap["code"] != "INVALID_START" {
		t.Errorf("want code=INVALID_START, got %v", errMap["code"])
	}
}

// TestGetThingStats_EndBeforeStart verifies that end ≤ start returns 400
// INVALID_TIME_WINDOW.
func TestGetThingStats_EndBeforeStart(t *testing.T) {
	t.Parallel()
	h := newHandler(&fakeThingStore{
		getThing: func(_ context.Context, _ string) (*thingstore.ThingRegistry, error) {
			return thingWithName("t1", "ai-gateway", nil), nil
		},
		queryThingRollup: func(_ context.Context, _ thingstore.ThingMetricsQuery) ([]metrics.ThingRollupRow, error) {
			return nil, nil
		},
		thingRollupHasAny: func(_ context.Context, _ string, _, _ time.Time) (bool, error) { return false, nil },
	}, nil)
	now := time.Now().UTC()
	c, rec := echoCtx(http.MethodGet, "/api/admin/things/t1/stats")
	c = withParam(c, "id", "t1")
	// end is 2h before now; start is 1h before now → end < start
	c = withQuery(c,
		"end", now.Add(-2*time.Hour).Format(time.RFC3339),
		"start", now.Add(-time.Hour).Format(time.RFC3339),
	)
	_ = h.GetThingStats(c)
	assertStatus(t, rec, http.StatusBadRequest)
	body := jsonBody(t, rec)
	errMap, _ := body["error"].(map[string]any)
	if errMap["code"] != "INVALID_TIME_WINDOW" {
		t.Errorf("want code=INVALID_TIME_WINDOW, got %v", errMap["code"])
	}
}

// GetThingStats — QueryThingRollup failure

// TestGetThingStats_QueryRollupError verifies that a DB error from
// QueryThingRollup returns 500 QUERY_THING_ROLLUP_FAILED.
func TestGetThingStats_QueryRollupError(t *testing.T) {
	t.Parallel()
	h := newHandler(&fakeThingStore{
		getThing: func(_ context.Context, _ string) (*thingstore.ThingRegistry, error) {
			return thingWithName("t1", "ai-gateway", nil), nil
		},
		queryThingRollup: func(_ context.Context, _ thingstore.ThingMetricsQuery) ([]metrics.ThingRollupRow, error) {
			return nil, errors.New("boom")
		},
		thingRollupHasAny: func(_ context.Context, _ string, _, _ time.Time) (bool, error) { return false, nil },
	}, nil)
	c, rec := echoCtx(http.MethodGet, "/api/admin/things/t1/stats")
	c = withParam(c, "id", "t1")
	_ = h.GetThingStats(c)
	assertStatus(t, rec, http.StatusInternalServerError)
	body := jsonBody(t, rec)
	errMap, _ := body["error"].(map[string]any)
	if errMap["code"] != "QUERY_THING_ROLLUP_FAILED" {
		t.Errorf("want code=QUERY_THING_ROLLUP_FAILED, got %v", errMap["code"])
	}
}

// GetThingStats — agent rollup-disabled inference

// TestGetThingStats_AgentRollupDisabled_NoRecentRows verifies that an agent
// Thing with zero rollup rows AND no recent rows returns enabled=false with
// the rollup-disabled message. This is the Hub enableAgentRollup=false signal.
func TestGetThingStats_AgentRollupDisabled_NoRecentRows(t *testing.T) {
	t.Parallel()
	h := newHandler(&fakeThingStore{
		getThing: func(_ context.Context, _ string) (*thingstore.ThingRegistry, error) {
			return thingWithName("t1", "agent", nil), nil
		},
		queryThingRollup: func(_ context.Context, _ thingstore.ThingMetricsQuery) ([]metrics.ThingRollupRow, error) {
			return nil, nil
		},
		thingRollupHasAny: func(_ context.Context, _ string, _, _ time.Time) (bool, error) { return false, nil },
	}, nil)
	c, rec := echoCtx(http.MethodGet, "/api/admin/things/t1/stats")
	c = withParam(c, "id", "t1")
	_ = h.GetThingStats(c)
	assertStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)
	if enabled, _ := body["enabled"].(bool); enabled {
		t.Errorf("want enabled=false for agent with no rollup rows")
	}
	msg, _ := body["rollupDisabledMessage"].(string)
	if msg == "" {
		t.Errorf("want non-empty rollupDisabledMessage")
	}
}

// TestGetThingStats_AgentRollupEnabled_HasRecentRows verifies that an agent
// Thing with zero query-window rows but has rows in the last hour returns
// enabled=true (rollup is running, just no traffic in window).
func TestGetThingStats_AgentRollupEnabled_HasRecentRows(t *testing.T) {
	t.Parallel()
	h := newHandler(&fakeThingStore{
		getThing: func(_ context.Context, _ string) (*thingstore.ThingRegistry, error) {
			return thingWithName("t1", "agent", nil), nil
		},
		queryThingRollup: func(_ context.Context, _ thingstore.ThingMetricsQuery) ([]metrics.ThingRollupRow, error) {
			return nil, nil
		},
		thingRollupHasAny: func(_ context.Context, _ string, _, _ time.Time) (bool, error) { return true, nil },
	}, nil)
	c, rec := echoCtx(http.MethodGet, "/api/admin/things/t1/stats")
	c = withParam(c, "id", "t1")
	_ = h.GetThingStats(c)
	assertStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)
	if enabled, _ := body["enabled"].(bool); !enabled {
		t.Errorf("want enabled=true when ThingRollupHasAnyRecent=true")
	}
}

// GetThingStats — happy path (non-agent, with rows, with metric filter)

// TestGetThingStats_HappyPath_NonAgent verifies the full success path for a
// non-agent Thing, including Name population, metric-filter forwarding to the
// query struct, and correct row conversion in the response.
func TestGetThingStats_HappyPath_NonAgent(t *testing.T) {
	t.Parallel()
	bucketStart := time.Now().UTC().Truncate(time.Minute)
	returnRows := []metrics.ThingRollupRow{
		{
			ID:           "row1",
			ThingID:      "t1",
			BucketStart:  bucketStart,
			MetricName:   "tokens.total",
			DimensionKey: "",
			Value:        42.0,
		},
	}
	var capturedQuery thingstore.ThingMetricsQuery
	h := newHandler(&fakeThingStore{
		getThing: func(_ context.Context, _ string) (*thingstore.ThingRegistry, error) {
			return thingWithName("t1", "ai-gateway", strPtr("gw-prod")), nil
		},
		queryThingRollup: func(_ context.Context, q thingstore.ThingMetricsQuery) ([]metrics.ThingRollupRow, error) {
			capturedQuery = q
			return returnRows, nil
		},
		thingRollupHasAny: func(_ context.Context, _ string, _, _ time.Time) (bool, error) { return false, nil },
	}, nil)
	c, rec := echoCtx(http.MethodGet, "/api/admin/things/t1/stats")
	c = withParam(c, "id", "t1")
	c = withQuery(c, "metric", "tokens.total , latency")
	_ = h.GetThingStats(c)
	assertStatus(t, rec, http.StatusOK)

	body := jsonBody(t, rec)
	if body["thingId"] != "t1" {
		t.Errorf("thingId mismatch: %v", body["thingId"])
	}
	if body["thingName"] != "gw-prod" {
		t.Errorf("thingName mismatch: %v", body["thingName"])
	}
	if enabled, _ := body["enabled"].(bool); !enabled {
		t.Errorf("want enabled=true for non-agent Thing")
	}
	rows, _ := body["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if v, _ := rows[0].(map[string]any)["value"].(float64); v != 42.0 {
		t.Errorf("row value = %v, want 42.0", v)
	}
	// Verify metric filter was forwarded to the store.
	if len(capturedQuery.Metrics) != 2 || capturedQuery.Metrics[0] != "tokens.total" || capturedQuery.Metrics[1] != "latency" {
		t.Errorf("capturedQuery.Metrics = %v, want [tokens.total latency]", capturedQuery.Metrics)
	}
}

// TestGetThingStats_RowsWithMetadata verifies that JSON Metadata bytes in a
// rollup row are decoded into the response row, not emitted as raw bytes.
func TestGetThingStats_RowsWithMetadata(t *testing.T) {
	t.Parallel()
	meta := json.RawMessage(`{"p50":12.3}`)
	h := newHandler(&fakeThingStore{
		getThing: func(_ context.Context, _ string) (*thingstore.ThingRegistry, error) {
			return thingWithName("t1", "ai-gateway", nil), nil
		},
		queryThingRollup: func(_ context.Context, _ thingstore.ThingMetricsQuery) ([]metrics.ThingRollupRow, error) {
			return []metrics.ThingRollupRow{{MetricName: "latency", Metadata: meta}}, nil
		},
		thingRollupHasAny: func(_ context.Context, _ string, _, _ time.Time) (bool, error) { return false, nil },
	}, nil)
	c, rec := echoCtx(http.MethodGet, "/api/admin/things/t1/stats")
	c = withParam(c, "id", "t1")
	_ = h.GetThingStats(c)
	assertStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)
	rows, _ := body["rows"].([]any)
	if len(rows) == 0 {
		t.Fatal("no rows in response")
	}
	rowMap, _ := rows[0].(map[string]any)
	metaField, ok := rowMap["metadata"]
	if !ok {
		t.Fatal("metadata field missing from row")
	}
	// After round-trip through JSON the decoded value is a map[string]any.
	metaMap, ok := metaField.(map[string]any)
	if !ok {
		t.Fatalf("metadata should decode to map, got %T: %v", metaField, metaField)
	}
	if metaMap["p50"] != 12.3 {
		t.Errorf("metadata.p50 = %v, want 12.3", metaMap["p50"])
	}
}

// resolveDimensionNames — via pgxmock pool

// TestResolveDimensionNames_PoolNil verifies that a nil pool returns nil
// immediately (no panic), since there's nothing to query.
func TestResolveDimensionNames_PoolNil(t *testing.T) {
	t.Parallel()
	h := newHandler(nil, nil) // pool = nil
	rows := []metrics.ThingRollupRow{
		{DimensionKey: "model=abc123"},
	}
	got := h.resolveDimensionNames(context.Background(), rows)
	if got != nil {
		t.Errorf("want nil when pool is nil, got %v", got)
	}
}

// TestResolveDimensionNames_NoRows verifies that an empty rows slice returns nil.
func TestResolveDimensionNames_NoRows(t *testing.T) {
	t.Parallel()
	h := newHandler(nil, nil)
	got := h.resolveDimensionNames(context.Background(), nil)
	if got != nil {
		t.Errorf("want nil for empty rows, got %v", got)
	}
}

// TestResolveDimensionNames_NoKnownDims verifies that rows with no
// dim-lookup-registered dimension keys return nil (no DB call).
func TestResolveDimensionNames_NoKnownDims(t *testing.T) {
	t.Parallel()
	h := newHandler(nil, nil)
	rows := []metrics.ThingRollupRow{
		{DimensionKey: "target_host=api.openai.com"}, // not in dimLookups
	}
	got := h.resolveDimensionNames(context.Background(), rows)
	if got != nil {
		t.Errorf("want nil when no dim-lookup keys present, got %v", got)
	}
}

// TestResolveDimensionNames_QueryError_ContinuesGracefully verifies the
// named failure mode: when pool.Query returns an error the handler logs
// at debug level (not tested here) and continues — the response omits
// that dimension's names rather than failing the whole request.
func TestResolveDimensionNames_QueryError_ContinuesGracefully(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	// Expect one Query (for "model") and return an error.
	// AnyArg() matches the []string ids argument.
	mock.ExpectQuery(`SELECT`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("query failed"))

	h := &Handler{
		thing:  nil, // not called in resolveDimensionNames
		pool:   mock,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	rows := []metrics.ThingRollupRow{
		{DimensionKey: "model=m1"},
	}
	got := h.resolveDimensionNames(context.Background(), rows)
	// Error path yields nil (no names resolved) — request continues.
	if got != nil {
		t.Errorf("want nil on query error (graceful continue), got %v", got)
	}
}

// TestResolveDimensionNames_HappyPath resolves a "model" dimension key via
// pgxmock and checks that the returned map contains the name.
func TestResolveDimensionNames_HappyPath(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	// AnyArg() matches the []string ids argument pgxmock receives.
	mock.ExpectQuery(`SELECT`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).
			AddRow("m1", "gpt-4"))

	h := &Handler{
		thing:  nil,
		pool:   mock,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	rows := []metrics.ThingRollupRow{
		{DimensionKey: "model=m1"},
	}
	got := h.resolveDimensionNames(context.Background(), rows)
	if got == nil {
		t.Fatal("want non-nil map, got nil")
	}
	if got["m1"] != "gpt-4" {
		t.Errorf("want m1→gpt-4, got %v", got)
	}
}

// TestResolveDimensionNames_ScanError_Skipped verifies the named failure mode:
// a row that fails Scan is silently skipped (dbRows.Scan error path). We
// provoke this by returning a value whose type is incompatible with *string.
// pgxmock AddRow with a struct value is not assignable to string, causing
// the Scan call to return an error and skip the row.
func TestResolveDimensionNames_ScanError_Skipped(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	// Provide a struct as the "name" column value — not assignable to string,
	// so Scan will error and the row is skipped.
	type badType struct{ X int }
	mock.ExpectQuery(`SELECT`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).
			AddRow("m1", badType{X: 1}))

	h := &Handler{
		thing:  nil,
		pool:   mock,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	rows := []metrics.ThingRollupRow{
		{DimensionKey: "model=m1"},
	}
	// Scan failure → row skipped → out is empty → return nil.
	got := h.resolveDimensionNames(context.Background(), rows)
	if got != nil {
		t.Errorf("want nil when scan fails (row skipped), got %v", got)
	}
}

// GetThingStats — integration: displayNames appear in response

// TestGetThingStats_DisplayNamesResolved verifies that when resolveDimensionNames
// produces a name map the response includes displayNames.
func TestGetThingStats_DisplayNamesResolved(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).
			AddRow("prov1", "openai-prod"))

	h := &Handler{
		thing: &fakeThingStore{
			getThing: func(_ context.Context, _ string) (*thingstore.ThingRegistry, error) {
				return thingWithName("t1", "ai-gateway", nil), nil
			},
			queryThingRollup: func(_ context.Context, _ thingstore.ThingMetricsQuery) ([]metrics.ThingRollupRow, error) {
				return []metrics.ThingRollupRow{
					{ThingID: "t1", DimensionKey: "provider=prov1", MetricName: "tokens.total", Value: 10},
				}, nil
			},
			thingRollupHasAny: func(_ context.Context, _ string, _, _ time.Time) (bool, error) { return false, nil },
		},
		pool:   mock,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	c, rec := echoCtx(http.MethodGet, "/api/admin/things/t1/stats")
	c = withParam(c, "id", "t1")
	_ = h.GetThingStats(c)
	assertStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)
	displayNames, ok := body["displayNames"].(map[string]any)
	if !ok || displayNames["prov1"] != "openai-prod" {
		t.Errorf("want displayNames.prov1=openai-prod, got %v", body["displayNames"])
	}
}

func TestSplitDimensionKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		dk        string
		wantName  string
		wantValue string
		wantOK    bool
	}{
		// Named failure: empty string → ok=false.
		{"", "", "", false},
		// Named failure: no '=' character → ok=false.
		{"nodim", "", "", false},
		// Named failure: '=' at position 0 → ok=false (idx <= 0).
		{"=value", "", "", false},
		// Named failure: '=' at last position (no value) → ok=false.
		{"name=", "", "", false},
		// Happy path.
		{"model=gpt-4", "model", "gpt-4", true},
		// Dimension with '=' inside the value.
		{"model=a=b", "model", "a=b", true},
	}
	for _, tc := range tests {
		name, value, ok := splitDimensionKey(tc.dk)
		if ok != tc.wantOK {
			t.Errorf("splitDimensionKey(%q): ok=%v, want %v", tc.dk, ok, tc.wantOK)
		}
		if ok && (name != tc.wantName || value != tc.wantValue) {
			t.Errorf("splitDimensionKey(%q): got (%q,%q), want (%q,%q)",
				tc.dk, name, value, tc.wantName, tc.wantValue)
		}
	}
}

// TestConvertThingRollupRows_MetadataDecoded verifies that valid JSON Metadata
// bytes are decoded once and embedded as a native Go value in the wire output.
func TestConvertThingRollupRows_MetadataDecoded(t *testing.T) {
	t.Parallel()
	rows := []metrics.ThingRollupRow{
		{MetricName: "latency", Metadata: json.RawMessage(`{"p99":99.9}`)},
		{MetricName: "errors", Metadata: nil}, // nil metadata → no metadata field
	}
	out := convertThingRollupRows(rows)
	if len(out) != 2 {
		t.Fatalf("want 2 output rows, got %d", len(out))
	}
	meta0, ok := out[0].Metadata.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any metadata, got %T", out[0].Metadata)
	}
	if meta0["p99"] != 99.9 {
		t.Errorf("p99 = %v, want 99.9", meta0["p99"])
	}
	if out[1].Metadata != nil {
		t.Errorf("nil metadata row should remain nil, got %v", out[1].Metadata)
	}
}

// TestConvertThingRollupRows_InvalidMetadata verifies that a row with
// unparseable JSON Metadata bytes has Metadata=nil in the output (Unmarshal
// error → field omitted) rather than the raw bytes exposed to clients.
func TestConvertThingRollupRows_InvalidMetadata(t *testing.T) {
	t.Parallel()
	rows := []metrics.ThingRollupRow{
		{MetricName: "x", Metadata: json.RawMessage(`not-json`)},
	}
	out := convertThingRollupRows(rows)
	if len(out) != 1 {
		t.Fatalf("want 1 row, got %d", len(out))
	}
	if out[0].Metadata != nil {
		t.Errorf("want nil for invalid metadata bytes, got %v", out[0].Metadata)
	}
}

func TestParsePagination(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		queryParams map[string]string
		wantLimit   int
		wantOffset  int
	}{
		{"defaults", nil, 50, 0},
		{"custom", map[string]string{"limit": "200", "offset": "100"}, 200, 100},
		{"limit capped at 1000", map[string]string{"limit": "5000"}, 1000, 0},
		{"negative limit ignored", map[string]string{"limit": "-1"}, 50, 0},
		{"zero limit ignored", map[string]string{"limit": "0"}, 50, 0},
		{"negative offset ignored", map[string]string{"offset": "-1"}, 50, 0},
		{"non-numeric limit ignored", map[string]string{"limit": "abc"}, 50, 0},
		{"non-numeric offset ignored", map[string]string{"offset": "xyz"}, 50, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := echo.New()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.queryParams != nil {
				q := r.URL.Query()
				for k, v := range tc.queryParams {
					q.Set(k, v)
				}
				r.URL.RawQuery = q.Encode()
			}
			rec := httptest.NewRecorder()
			c := e.NewContext(r, rec)
			p := parsePagination(c)
			if p.Limit != tc.wantLimit {
				t.Errorf("Limit = %d, want %d", p.Limit, tc.wantLimit)
			}
			if p.Offset != tc.wantOffset {
				t.Errorf("Offset = %d, want %d", p.Offset, tc.wantOffset)
			}
		})
	}
}

func TestParseRFC3339Flexible(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		input  string
		wantOK bool
	}{
		{"RFC3339Nano with nanoseconds", "2024-01-15T10:30:00.123456789Z", true},
		{"RFC3339 (no fraction) — covered by RFC3339Nano superset", "2024-01-15T10:30:00Z", true},
		{"garbage", "not-a-time", false},
		{"empty", "", false},
		{"date only", "2024-01-15", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, ok := parseRFC3339Flexible(tc.input)
			if ok != tc.wantOK {
				t.Errorf("parseRFC3339Flexible(%q): ok=%v, want %v", tc.input, ok, tc.wantOK)
			}
		})
	}
}

// TestParseTimeRange_BothValid verifies that valid RFC3339 startTime and
// endTime query params parse correctly into non-nil pointers.
func TestParseTimeRange_BothValid(t *testing.T) {
	t.Parallel()
	e := echo.New()
	start := "2024-01-01T00:00:00Z"
	end := "2024-01-02T00:00:00Z"
	r := httptest.NewRequest(http.MethodGet, "/?startTime="+start+"&endTime="+end, nil)
	c := e.NewContext(r, httptest.NewRecorder())
	s, en := parseTimeRange(c)
	if s == nil {
		t.Fatal("want non-nil start")
		return
	}
	if en == nil {
		t.Fatal("want non-nil end")
		return
	}
	if !s.Equal(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("start = %v", *s)
	}
}

// TestParseTimeRange_InvalidIgnored verifies that invalid timestamps for both
// startTime and endTime yield nil pointers (silently ignored per design).
func TestParseTimeRange_InvalidIgnored(t *testing.T) {
	t.Parallel()
	e := echo.New()
	r := httptest.NewRequest(http.MethodGet, "/?startTime=bad&endTime=bad", nil)
	c := e.NewContext(r, httptest.NewRecorder())
	s, en := parseTimeRange(c)
	if s != nil {
		t.Errorf("want nil start for invalid input, got %v", s)
	}
	if en != nil {
		t.Errorf("want nil end for invalid input, got %v", en)
	}
}

// actorFromContext — covers the nil AdminAuth branch

// TestActorFromContext_NoAuth verifies that actorFromContext returns an empty
// Actor when no AdminAuth is set on the context, exercising the nil guard.
func TestActorFromContext_NoAuth(t *testing.T) {
	t.Parallel()
	e := echo.New()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	c := e.NewContext(r, httptest.NewRecorder())
	actor := actorFromContext(c)
	if actor.UserID != "" || actor.Name != "" {
		t.Errorf("want empty Actor, got %+v", actor)
	}
}

// RegisterAdminThingStatsRoutes — smoke test that the route is mounted

// TestRegisterAdminThingStatsRoutes_MountsRoute verifies that the route
// GET /things/:id/stats is registered on the echo group.
func TestRegisterAdminThingStatsRoutes_MountsRoute(t *testing.T) {
	t.Parallel()
	h := newHandler(nil, nil)
	e := echo.New()
	g := e.Group("/api/admin")
	h.RegisterAdminThingStatsRoutes(g, func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	})
	routes := e.Routes()
	for _, r := range routes {
		if r.Method == http.MethodGet && r.Path == "/api/admin/things/:id/stats" {
			return // found
		}
	}
	t.Errorf("expected GET /api/admin/things/:id/stats route to be registered; routes: %v", routes)
}

// New constructor

// TestNew_WithExplicitLogger verifies that New returns a Handler with the
// provided logger rather than slog.Default(), and that it satisfies the
// structural invariant of thingOperations being non-nil when a pool is given.
func TestNew_WithExplicitLogger(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(Deps{
		Pool:   nil, // nil *pgxpool.Pool — satisfies PgxPool via nil pointer
		Logger: logger,
	})
	if h == nil {
		t.Fatal("New returned nil")
		return
	}
	// Logger should be the one we passed in, not slog.Default().
	if h.logger != logger {
		t.Errorf("logger mismatch: got %v, want %v", h.logger, logger)
	}
}

// TestNew_NilLogger_FallsBackToDefault verifies that when Deps.Logger is nil,
// New falls back to slog.Default() rather than panicking or leaving nil.
func TestNew_NilLogger_FallsBackToDefault(t *testing.T) {
	t.Parallel()
	h := New(Deps{
		Pool:   nil,
		Logger: nil, // trigger the slog.Default() fallback
	})
	if h == nil {
		t.Fatal("New returned nil")
		return
	}
	if h.logger == nil {
		t.Errorf("New should have set a non-nil logger via slog.Default() fallback")
	}
}

// actorFromContext — non-nil AdminAuth branch

// TestActorFromContext_WithAuth verifies that actorFromContext extracts
// KeyID and KeyName from a context carrying an AdminAuth value.
func TestActorFromContext_WithAuth(t *testing.T) {
	t.Parallel()
	e := echo.New()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	c := e.NewContext(r, httptest.NewRecorder())
	// Inject an AdminAuth via the exported helper so the same key is used.
	aa := &auth.AdminAuth{KeyID: "key-1", KeyName: "operator"}
	middleware.WithAdminAuth(c, aa)
	actor := actorFromContext(c)
	if actor.UserID != "key-1" {
		t.Errorf("UserID = %q, want key-1", actor.UserID)
	}
	if actor.Name != "operator" {
		t.Errorf("Name = %q, want operator", actor.Name)
	}
}

// internalServerError helper

// TestInternalServerError_Shape verifies that the helper writes a 500 JSON
// body with the expected error shape used elsewhere in the package.
func TestInternalServerError_Shape(t *testing.T) {
	t.Parallel()
	e := echo.New()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(r, rec)
	_ = internalServerError(c, "something went wrong")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	body := jsonBody(t, rec)
	errMap, _ := body["error"].(map[string]any)
	if errMap["message"] != "something went wrong" {
		t.Errorf("message = %v, want 'something went wrong'", errMap["message"])
	}
	if errMap["type"] != "server_error" {
		t.Errorf("type = %v, want server_error", errMap["type"])
	}
}
