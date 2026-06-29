package core

import "github.com/prometheus/client_golang/prometheus"

// Observability for the content-scan hot path (spec §8). The benign
// short-circuit rate — the fraction of scans where no pattern fired — is the
// headline health signal: a low rate flags either a match-dense / abusive
// workload or a misconfigured rule set putting unexpected CPU on the data path.
// Labelled by hook implementation so operators can see it per content hook.
var (
	ContentScanTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "hooks",
		Name:      "content_scan_total",
		Help:      "Content-scan executions on the Matcher path, by hook implementation.",
	}, []string{"impl"})

	ContentScanBenignTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "hooks",
		Name:      "content_scan_benign_total",
		Help:      "Content scans where no pattern matched (the short-circuit hot path), by hook implementation.",
	}, []string{"impl"})
)

func init() {
	// Register on the default registry the data-plane /metrics endpoints read.
	// MustRegister panics on duplicate registration; this package is imported
	// once per process so init runs once.
	prometheus.DefaultRegisterer.MustRegister(ContentScanTotal, ContentScanBenignTotal)
}

// ObserveContentScan records one content-scan execution and whether it was
// benign (no pattern fired). Call once per Execute on the Matcher path.
func ObserveContentScan(impl string, matched int) {
	ContentScanTotal.WithLabelValues(impl).Inc()
	if matched == 0 {
		ContentScanBenignTotal.WithLabelValues(impl).Inc()
	}
}
