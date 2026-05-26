package analytics

import (
	"errors"
	"net/http"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

func TestLatencyPhasesGroupColumn_All(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		ok   bool
		want string
	}{
		{"provider", true, "COALESCE(routed_provider_name, provider_name, 'unknown')"},
		{"model", true, "COALESCE(routed_model_name, model_name, 'unknown')"},
		{"virtual_key", true, "COALESCE((identity->'vk'->>'name'), 'unknown')"},
		{"node", true, "COALESCE(thing_name, thing_id, 'unknown')"},
		{"host", true, "COALESCE(target_host, 'unknown')"},
		{"device", true, "COALESCE(thing_name, source_ip, 'unknown')"},
		{"", false, ""},
		{"nope", false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := latencyPhasesGroupColumn(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Errorf("got=%q ok=%v, want=%q ok=%v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestAnalyticsLatencyPhases_MissingGroupBy(t *testing.T) {
	t.Parallel()
	_, h := newMockHandler(t)
	c, rec := echoCtx("GET", "/api/admin/analytics/latency-phases")
	if err := h.AnalyticsLatencyPhases(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusBadRequest)
	body := jsonBody(t, rec)
	e, _ := body["error"].(map[string]any)
	if e["type"] != "missing_groupBy" {
		t.Errorf("want type=missing_groupBy, got=%v", body)
	}
}

func TestAnalyticsLatencyPhases_UnsupportedGroupBy(t *testing.T) {
	t.Parallel()
	_, h := newMockHandler(t)
	c, rec := echoCtx("GET", "/api/admin/analytics/latency-phases?groupBy=bogus")
	if err := h.AnalyticsLatencyPhases(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusBadRequest)
	body := jsonBody(t, rec)
	e, _ := body["error"].(map[string]any)
	if e["type"] != "bad_groupBy" {
		t.Errorf("want type=bad_groupBy, got=%v", body)
	}
}

func TestAnalyticsLatencyPhases_MissingWindow(t *testing.T) {
	t.Parallel()
	_, h := newMockHandler(t)
	c, rec := echoCtx("GET", "/api/admin/analytics/latency-phases?groupBy=provider")
	if err := h.AnalyticsLatencyPhases(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusBadRequest)
	body := jsonBody(t, rec)
	e, _ := body["error"].(map[string]any)
	if e["type"] != "missing_window" {
		t.Errorf("want type=missing_window, got=%v", body)
	}
}

func TestAnalyticsLatencyPhases_QueryError(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	mock.ExpectQuery(`FROM   traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("conn lost"))

	c, rec := echoCtx("GET",
		"/api/admin/analytics/latency-phases?groupBy=provider&start=2026-01-01T00:00:00Z&end=2026-01-02T00:00:00Z")
	if err := h.AnalyticsLatencyPhases(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusInternalServerError)
}

func TestAnalyticsLatencyPhases_HappyAllSource(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	cols := []string{
		"group_key", "request_count",
		"total_p50", "total_p95", "total_p99",
		"us_p50", "us_p95", "us_p99",
		"ttfb_p50", "ttfb_p95", "ttfb_p99",
		"upt_p50", "upt_p95", "upt_p99",
		"rh_p50", "rh_p95",
		"sh_p50", "sh_p95",
	}
	p50, p95, p99 := 10, 20, 30
	mock.ExpectQuery(`FROM   traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(cols).
			AddRow("OpenAI", int64(42),
				&p50, &p95, &p99,
				&p50, &p95, &p99,
				&p50, &p95, &p99,
				&p50, &p95, &p99,
				&p50, &p95,
				&p50, &p95))

	c, rec := echoCtx("GET",
		"/api/admin/analytics/latency-phases?groupBy=provider&start=2026-01-01T00:00:00Z&end=2026-01-02T00:00:00Z")
	if err := h.AnalyticsLatencyPhases(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)
	rows, _ := body["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d (body=%s)", len(rows), rec.Body.String())
	}
	row := rows[0].(map[string]any)
	if row["groupKey"] != "OpenAI" {
		t.Errorf("groupKey=%v", row["groupKey"])
	}
	if row["groupLabel"] != "OpenAI" {
		t.Errorf("groupLabel=%v", row["groupLabel"])
	}
	if row["requestCount"] != float64(42) {
		t.Errorf("requestCount=%v", row["requestCount"])
	}
}

func TestAnalyticsLatencyPhases_SourceFilter(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// With source=ai-gateway, the query carries a third arg.
	mock.ExpectQuery(`FROM   traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "ai-gateway").
		WillReturnRows(pgxmock.NewRows([]string{
			"group_key", "request_count",
			"total_p50", "total_p95", "total_p99",
			"us_p50", "us_p95", "us_p99",
			"ttfb_p50", "ttfb_p95", "ttfb_p99",
			"upt_p50", "upt_p95", "upt_p99",
			"rh_p50", "rh_p95",
			"sh_p50", "sh_p95",
		}))

	c, rec := echoCtx("GET",
		"/api/admin/analytics/latency-phases?groupBy=model&start=2026-01-01T00:00:00Z&end=2026-01-02T00:00:00Z&source=ai-gateway")
	if err := h.AnalyticsLatencyPhases(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
}

func TestAnalyticsLatencyPhases_ScanError(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// Row with wrong column count → scan fails → handler returns 500.
	mock.ExpectQuery(`FROM   traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"only_one_col"}).AddRow("oops"))

	c, rec := echoCtx("GET",
		"/api/admin/analytics/latency-phases?groupBy=node&start=2026-01-01T00:00:00Z&end=2026-01-02T00:00:00Z")
	if err := h.AnalyticsLatencyPhases(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusInternalServerError)
}
