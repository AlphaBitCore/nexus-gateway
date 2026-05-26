package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type fakeShadowProbe struct {
	age      time.Duration
	stale    time.Duration
	reported bool
}

func (f *fakeShadowProbe) LastReportAge() time.Duration { return f.age }
func (f *fakeShadowProbe) StaleAfter() time.Duration    { return f.stale }
func (f *fakeShadowProbe) HasReported() bool            { return f.reported }

func TestHealthz_Ready(t *testing.T) {
	ready := &atomic.Bool{}
	ready.Store(true)

	handler := NewHandler(ready, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d", rec.Code, http.StatusOK)
	}

	var body statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q; want %q", body.Status, "ok")
	}

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q; want %q", ct, "application/json")
	}
}

func TestHealthz_ShuttingDown(t *testing.T) {
	ready := &atomic.Bool{} // default false

	handler := NewHandler(ready, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want %d", rec.Code, http.StatusServiceUnavailable)
	}

	var body statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Status != "shutting_down" {
		t.Errorf("status = %q; want %q", body.Status, "shutting_down")
	}
}

func TestMetrics_ReturnsPrometheusFormat(t *testing.T) {
	ready := &atomic.Bool{}
	ready.Store(true)

	handler := NewHandler(ready, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.String()
	// Prometheus exposition format always contains at least go runtime metrics
	if !strings.Contains(body, "go_goroutines") {
		t.Error("/metrics response missing expected Prometheus metric go_goroutines")
	}

	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/plain") && !strings.Contains(ct, "text/openmetrics") {
		// promhttp may return text/plain or openmetrics depending on accept header
		t.Errorf("Content-Type = %q; want text/plain or openmetrics", ct)
	}
}

func TestReadyz_NoProbe_BehavesLikeHealthz(t *testing.T) {
	ready := &atomic.Bool{}
	ready.Store(true)

	handler := NewHandler(ready, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d", rec.Code, http.StatusOK)
	}
}

func TestReadyz_ShuttingDown(t *testing.T) {
	ready := &atomic.Bool{} // false

	handler := NewHandler(ready, &fakeShadowProbe{reported: true, age: 1 * time.Second, stale: 60 * time.Second}, nil)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestReadyz_ShadowFresh_ReturnsOK(t *testing.T) {
	ready := &atomic.Bool{}
	ready.Store(true)

	handler := NewHandler(ready, &fakeShadowProbe{reported: true, age: 5 * time.Second, stale: 30 * time.Second}, nil)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d", rec.Code, http.StatusOK)
	}

	var body readyzResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q; want %q", body.Status, "ok")
	}
	if body.ShadowAgeSeconds != 5 {
		t.Errorf("ShadowAgeSeconds = %v, want 5", body.ShadowAgeSeconds)
	}
}

func TestReadyz_ShadowStale_Returns503(t *testing.T) {
	ready := &atomic.Bool{}
	ready.Store(true)

	handler := NewHandler(ready, &fakeShadowProbe{reported: true, age: 120 * time.Second, stale: 30 * time.Second}, nil)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want %d", rec.Code, http.StatusServiceUnavailable)
	}

	var body readyzResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Status != "shadow_stale" {
		t.Errorf("status = %q; want %q", body.Status, "shadow_stale")
	}
}

func TestReadyz_NeverReported_Returns503(t *testing.T) {
	ready := &atomic.Bool{}
	ready.Store(true)

	handler := NewHandler(ready, &fakeShadowProbe{reported: false, stale: 30 * time.Second}, nil)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want %d", rec.Code, http.StatusServiceUnavailable)
	}

	var body readyzResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Status != "no_shadow_report" {
		t.Errorf("status = %q; want %q", body.Status, "no_shadow_report")
	}
}

func TestShadowAgeGauge_RegisteredWhenProbeProvided(t *testing.T) {
	ready := &atomic.Bool{}
	ready.Store(true)
	reg := prometheus.NewRegistry()

	probe := &fakeShadowProbe{reported: true, age: 42 * time.Second, stale: 60 * time.Second}
	NewHandler(ready, probe, reg)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var got *float64
	for _, mf := range mfs {
		if mf.GetName() == "compliance_proxy_shadow_last_report_age_seconds" {
			for _, m := range mf.GetMetric() {
				v := m.GetGauge().GetValue()
				got = &v
			}
		}
	}
	if got == nil {
		t.Fatalf("gauge compliance_proxy_shadow_last_report_age_seconds not registered")
	}
	if *got != 42 {
		t.Errorf("gauge = %v, want 42", *got)
	}
}
