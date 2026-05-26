package freshness

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// freshnessMetrics holds the Prometheus instruments for the freshness detector.
// A nil *freshnessMetrics is safe — all methods are no-ops on a nil receiver.
type freshnessMetrics struct {
	// skipsTotal counts requests that were detected as time-sensitive and
	// therefore skipped from the cache. Labels:
	//   rule_id  — the ID of the rule that fired (e.g. "stock-price").
	//   language — the language tag from the matching rule, or "any" when the
	//              rule's Languages list is empty.
	skipsTotal *prometheus.CounterVec
}

// newFreshnessMetrics registers nexus_cache_freshness_skips_total under the
// given Prometheus namespace using the supplied registerer.
//
// namespace is typically the service-level namespace (e.g. "nexus_aigw").
// reg must not be nil; pass prometheus.NewRegistry() in tests to isolate from
// the default registry.
func newFreshnessMetrics(namespace string, reg prometheus.Registerer) *freshnessMetrics {
	f := promauto.With(reg)
	return &freshnessMetrics{
		skipsTotal: f.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: namespace,
				Name:      "cache_freshness_skips_total",
				Help:      "Total number of cache skips caused by the freshness (time-sensitive) detector, broken down by rule and language.",
			},
			[]string{"rule_id", "language"},
		),
	}
}

// recordSkip increments the skips counter for the given rule. language is the
// first entry in Rule.Languages, or "any" when the list is empty.
func (m *freshnessMetrics) recordSkip(ruleID, language string) {
	if m == nil {
		return
	}
	m.skipsTotal.WithLabelValues(ruleID, language).Inc()
}

// ruleLanguageLabel returns the label value to use for the language dimension.
// If the rule's Languages slice is empty the rule applies to all languages and
// the label is set to "any".
func ruleLanguageLabel(languages []string) string {
	if len(languages) == 0 {
		return "any"
	}
	// Use the first language as the label. Multi-language rules carry both
	// "en" and "zh" — in practice these rules are bilingual seed rules and
	// "en" is the conventional primary language.
	return languages[0]
}
