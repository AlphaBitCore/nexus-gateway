package instruments

import "strings"

// Traffic error-class rollup series. The Hub's rollup-5m job emits one
// counter row per (error class, source domain) per bucket so the Control
// Plane's error-governance view can aggregate large windows from rollup
// tables instead of scanning raw traffic_event rows.
const (
	// MetricTrafficErrorClassCount counts failed data-plane requests per
	// error class per bucket. Sum-aggregated (registry default).
	MetricTrafficErrorClassCount = "traffic.error_class.count"
	// MetricTrafficErrorClassSeen carries first/last-seen TimestampMeta per
	// error class per bucket. Registered AggregationTimestamp so the merge
	// cascade folds MIN(first)/MAX(last) instead of summing.
	MetricTrafficErrorClassSeen = "traffic.error_class.seen"
	// DimensionErrorClass is the dimension name for the composite error
	// class key.
	DimensionErrorClass = "error_class"
	// ErrorClassOverflowCode is the sentinel errorCode of the per-bucket
	// overflow class: when a bucket holds more distinct error classes than
	// the producer's cap, the excess classes fold into one row per status
	// range with this code so the total count stays exact.
	ErrorClassOverflowCode = "__overflow__"
)

// ErrorClass is the decoded composite error-class identity. Provider and
// Model are the values stamped on the traffic_event row (routed name falling
// back to requested name) — deliberately NOT catalog UUIDs: early rejections
// (auth, routing no-match) carry no resolved IDs, and the class key must be
// byte-identical to the direct traffic_event GROUP BY so the rollup and
// direct read paths agree.
type ErrorClass struct {
	ErrorCode   string
	StatusRange string // "4xx" | "5xx"
	Provider    string
	Model       string
}

// escapeErrorClassPart makes a part safe to join with '|': '%' first so the
// escape marker itself round-trips, then '|'.
func escapeErrorClassPart(s string) string {
	s = strings.ReplaceAll(s, "%", "%25")
	return strings.ReplaceAll(s, "|", "%7C")
}

func unescapeErrorClassPart(s string) string {
	s = strings.ReplaceAll(s, "%7C", "|")
	return strings.ReplaceAll(s, "%25", "%")
}

// BuildErrorClassValue encodes the composite dimension value
// "code|range|provider|model" with '%'/'|' escaped per part. The result is
// passed to BuildDimensionKey(DimensionErrorClass, value); StatusRange is
// always non-empty so the value never collapses to "".
func BuildErrorClassValue(c ErrorClass) string {
	return escapeErrorClassPart(c.ErrorCode) + "|" +
		escapeErrorClassPart(c.StatusRange) + "|" +
		escapeErrorClassPart(c.Provider) + "|" +
		escapeErrorClassPart(c.Model)
}

// ParseErrorClassValue decodes a composite value produced by
// BuildErrorClassValue. Returns false when the value does not have exactly
// four parts (foreign or corrupted dimension rows must be skipped, not
// misattributed).
func ParseErrorClassValue(v string) (ErrorClass, bool) {
	parts := strings.Split(v, "|")
	if len(parts) != 4 {
		return ErrorClass{}, false
	}
	return ErrorClass{
		ErrorCode:   unescapeErrorClassPart(parts[0]),
		StatusRange: unescapeErrorClassPart(parts[1]),
		Provider:    unescapeErrorClassPart(parts[2]),
		Model:       unescapeErrorClassPart(parts[3]),
	}, true
}
