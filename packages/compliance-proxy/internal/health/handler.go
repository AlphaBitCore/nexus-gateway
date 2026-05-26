// Package health provides HTTP endpoints for liveness/readiness probes and
// Prometheus metrics scraping.
package health

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// statusResponse is the JSON body returned by /healthz.
type statusResponse struct {
	Status string `json:"status"`
}

// readyzResponse is the JSON body returned by /readyz. ShadowAgeSeconds is
// the floor-seconds age of the last shadow report; it is emitted even when
// the probe reports the shadow as stale so dashboards can display the
// observed age rather than guessing.
type readyzResponse struct {
	Status           string  `json:"status"`
	ShadowAgeSeconds float64 `json:"shadowAgeSeconds,omitempty"`
	ShadowStaleAfter float64 `json:"shadowStaleAfterSeconds,omitempty"`
	NoShadowReport   bool    `json:"noShadowReport,omitempty"`
}

// ShadowProbe lets callers expose enough shadow-sync state for readiness
// decisions and the staleness gauge. Production wires it to the
// *thingclient.Client (via a small adapter in main), which is why this
// interface lives in the health package rather than pulling thingclient in.
//
// HasReported discriminates "never reported" from "reported long ago": the
// former is the expected initial state and should not surface a stale-age
// gauge value, the latter is an incident.
type ShadowProbe interface {
	HasReported() bool
	LastReportAge() time.Duration
	StaleAfter() time.Duration
}

// NewHandler returns an http.ServeMux serving /healthz, /readyz, and
// /metrics.
//
// readiness controls the shutdown gate for both /healthz and /readyz; a
// store of false (set during graceful drain) returns 503 from both.
//
// probe is optional. When non-nil, /readyz additionally requires the shadow
// to have reported at least once AND age <= StaleAfter; a non-nil registerer
// also registers the Prometheus gauge
// compliance_proxy_shadow_last_report_age_seconds, which is sampled from
// the probe on every scrape.
//
// Passing nil for probe reduces /readyz to the same liveness check as
// /healthz — useful for ops contexts where the shadow client is not wired
// (unit tests, standalone smoke builds).
func NewHandler(readiness *atomic.Bool, probe ShadowProbe, reg prometheus.Registerer) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		var resp statusResponse
		if readiness.Load() {
			resp.Status = "ok"
			w.WriteHeader(http.StatusOK)
		} else {
			resp.Status = "shutting_down"
			w.WriteHeader(http.StatusServiceUnavailable)
		}

		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if !readiness.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(readyzResponse{Status: "shutting_down"})
			return
		}

		if probe == nil {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(readyzResponse{Status: "ok"})
			return
		}

		staleAfter := probe.StaleAfter().Seconds()

		if !probe.HasReported() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(readyzResponse{
				Status:           "no_shadow_report",
				NoShadowReport:   true,
				ShadowStaleAfter: staleAfter,
			})
			return
		}

		age := probe.LastReportAge()
		if age > probe.StaleAfter() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(readyzResponse{
				Status:           "shadow_stale",
				ShadowAgeSeconds: age.Seconds(),
				ShadowStaleAfter: staleAfter,
			})
			return
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(readyzResponse{
			Status:           "ok",
			ShadowAgeSeconds: age.Seconds(),
			ShadowStaleAfter: staleAfter,
		})
	})

	mux.Handle("/metrics", promhttp.Handler())

	if probe != nil && reg != nil {
		gauge := prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Namespace: "compliance_proxy",
				Name:      "shadow_last_report_age_seconds",
				Help:      "Seconds since the last successful shadow_report. 0 when no report has been sent yet.",
			},
			func() float64 {
				if !probe.HasReported() {
					return 0
				}
				return probe.LastReportAge().Seconds()
			},
		)
		reg.MustRegister(gauge)
	}

	return mux
}
