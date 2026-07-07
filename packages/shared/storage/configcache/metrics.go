package configcache

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Load-health metrics. A degraded push channel plus a failing database means
// the config plane is frozen on its previous snapshot — before these series
// existed that state was logs-only and invisible to alerting. Operators alert
// on loadFailures increasing, or on now() − lastLoadSuccess exceeding twice
// the service's TTL backstop.
var (
	loadFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nexus_configcache_load_failures_total",
		Help: "Snapshot loads that failed; the previous snapshot stays active.",
	}, []string{"cache"})

	lastLoadSuccess = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nexus_configcache_last_success_timestamp_seconds",
		Help: "Unix time of the last successful snapshot load.",
	}, []string{"cache"})
)
