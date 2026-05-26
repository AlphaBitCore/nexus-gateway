package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	cpmetrics "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	metricsreg "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// counterValue returns the post-Inc value of the counter that has all the
// supplied label values present. Tests use it to assert specific
// {method,route_class,status_class} tuples were incremented.
func counterValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if matchesLabels(m, labels) {
				if m.Counter != nil {
					return m.Counter.GetValue()
				}
			}
		}
	}
	return 0
}

func matchesLabels(m *dto.Metric, want map[string]string) bool {
	got := make(map[string]string, len(m.GetLabel()))
	for _, lp := range m.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func histogramObservationCount(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) uint64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if matchesLabels(m, labels) {
				if m.Histogram != nil {
					return m.Histogram.GetSampleCount()
				}
			}
		}
	}
	return 0
}

// TestRequestMetrics_IncrementsCounters covers the happy path: for each
// status class, the counter for {method, route_class, status_class}
// goes up by one and the duration histogram receives one observation.
// route_class must be the registered Echo template, never the concrete
// URL — verified by hitting /admin/users/:id with a concrete id and
// asserting the counter's route_class label is "/admin/users/:id".
func TestRequestMetrics_IncrementsCounters(t *testing.T) {
	// NOTE: package-level Register touches global vars in
	// control-plane/internal/metrics, so this test isn't t.Parallel.
	promReg := prometheus.NewRegistry()
	cpmetrics.Register(metricsreg.NewRegistry(promReg))

	e := echo.New()
	e.HideBanner = true
	e.Use(middleware.RequestMetrics())
	e.GET("/admin/users/:id", func(c echo.Context) error {
		return c.NoContent(http.StatusOK)
	})
	e.POST("/admin/users", func(c echo.Context) error {
		return c.NoContent(http.StatusCreated)
	})
	e.GET("/missing", func(c echo.Context) error {
		return c.NoContent(http.StatusNotFound)
	})
	e.GET("/oops", func(c echo.Context) error {
		return c.NoContent(http.StatusInternalServerError)
	})

	cases := []struct {
		method      string
		url         string
		routeClass  string
		statusClass string
	}{
		{http.MethodGet, "/admin/users/u_abc123", "/admin/users/:id", "2xx"},
		{http.MethodPost, "/admin/users", "/admin/users", "2xx"},
		{http.MethodGet, "/missing", "/missing", "4xx"},
		{http.MethodGet, "/oops", "/oops", "5xx"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.url, nil)
		e.ServeHTTP(rec, req)
	}

	for _, tc := range cases {
		got := counterValue(t, promReg, "nexus_http_requests_total", map[string]string{
			"method":       tc.method,
			"route_class":  tc.routeClass,
			"status_class": tc.statusClass,
		})
		if got != 1 {
			t.Errorf("counter for %s %s status=%s: got=%v want=1",
				tc.method, tc.routeClass, tc.statusClass, got)
		}
		hc := histogramObservationCount(t, promReg, "nexus_http_duration_ms", map[string]string{
			"route_class": tc.routeClass,
		})
		if hc < 1 {
			t.Errorf("histogram for %s observations=%d want>=1", tc.routeClass, hc)
		}
	}
}

// TestRequestMetrics_UnknownRouteFallsBackToUnknown covers the
// `routeClass == ""` branch — Echo leaves c.Path() empty when no route
// is registered, and the middleware must collapse that onto a single
// "unknown" label rather than fan out to one label per concrete URL
// (cardinality bomb). Echo's default HTTPErrorHandler runs after the
// middleware return so c.Response().Status defaults to 200 inside the
// middleware on an unmatched route; the route_class is "unknown" and
// the status_class lands in "2xx" — the load-bearing assertion is the
// route_class collapse, since that's the cardinality control.
func TestRequestMetrics_UnknownRouteFallsBackToUnknown(t *testing.T) {
	promReg := prometheus.NewRegistry()
	cpmetrics.Register(metricsreg.NewRegistry(promReg))

	e := echo.New()
	e.HideBanner = true
	e.Use(middleware.RequestMetrics())
	// No routes registered → c.Path()=="" inside the middleware. The
	// echo.NotFoundHandler returns an error but does not call
	// WriteHeader, so c.Response().Status stays at the default 200.

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/does/not/exist", nil)
	e.ServeHTTP(rec, req)

	got := counterValue(t, promReg, "nexus_http_requests_total", map[string]string{
		"method":       "GET",
		"route_class":  "unknown",
		"status_class": "2xx",
	})
	if got != 1 {
		t.Errorf("expected route_class=unknown counter to increment; got=%v", got)
	}
}

// TestRequestMetrics_NilInstrumentsNoPanic covers the `metrics.RequestsTotal != nil`
// guards: if Register has not been called the middleware must still pass
// the request through without panicking — important for ad-hoc test
// harnesses that mount the middleware but never call Register.
func TestRequestMetrics_NilInstrumentsNoPanic(t *testing.T) {
	// Reset package-level vars to nil to simulate "Register not called".
	prevReq := cpmetrics.RequestsTotal
	prevDur := cpmetrics.RequestDurationMs
	cpmetrics.RequestsTotal = nil
	cpmetrics.RequestDurationMs = nil
	t.Cleanup(func() {
		cpmetrics.RequestsTotal = prevReq
		cpmetrics.RequestDurationMs = prevDur
	})

	e := echo.New()
	e.HideBanner = true
	e.Use(middleware.RequestMetrics())
	e.GET("/", func(c echo.Context) error {
		return c.NoContent(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
}

// TestRequestMetrics_RealStatusBuckets covers the full live status-code
// classifier via the middleware. 1xx (Continue), 2xx, 3xx, 4xx, 5xx
// — each must land in its own labelled counter. 0/negative are exercised
// only via the in-package TestStatusBucket_Internal test (Echo's
// response writer rejects invalid HTTP codes at WriteHeader time, so
// they can't be reached through a real handler).
func TestRequestMetrics_RealStatusBuckets(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusContinue, "1xx"},
		{http.StatusOK, "2xx"},
		{http.StatusMovedPermanently, "3xx"},
		{http.StatusBadRequest, "4xx"},
		{http.StatusInternalServerError, "5xx"},
	}
	for _, tc := range cases {

		t.Run("", func(t *testing.T) {
			promReg := prometheus.NewRegistry()
			cpmetrics.Register(metricsreg.NewRegistry(promReg))
			e := echo.New()
			e.HideBanner = true
			e.Use(middleware.RequestMetrics())
			e.GET("/p", func(c echo.Context) error {
				c.Response().WriteHeader(tc.status)
				return nil
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/p", nil)
			e.ServeHTTP(rec, req)
			got := counterValue(t, promReg, "nexus_http_requests_total", map[string]string{
				"method":       "GET",
				"route_class":  "/p",
				"status_class": tc.want,
			})
			if got != 1 {
				t.Errorf("status=%d: counter for status_class=%s got=%v want=1", tc.status, tc.want, got)
			}
		})
	}
}
