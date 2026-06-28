package instruments

import (
	"github.com/goccy/go-json"
	"strings"
)

// IsHistogramMetric returns true if the metric stores histogram data in its
// metadata field (JSON-encoded Histogram). Derived from the aggregation-kind
// registry so there is a single source of truth for per-metric merge behavior.
func IsHistogramMetric(name string) bool {
	return AggregationKindFor(name) == AggregationHistogram
}

// IsTimestampMetric returns true if the metric stores first_seen/last_seen
// timestamps in its metadata field (JSON-encoded TimestampMeta). Derived from
// the aggregation-kind registry (see IsHistogramMetric).
func IsTimestampMetric(name string) bool {
	return AggregationKindFor(name) == AggregationTimestamp
}

// rowKey is the composite key used for grouping rollup rows during merge.
type rowKey struct {
	MetricName   string
	DimensionKey string
	SubDimension string
}

// thingRowKey is the composite key for thing rollup row merging — same as
// rowKey but with ThingID prepended so per-Thing rows never collide across
// Things during merge.
type thingRowKey struct {
	ThingID      string
	MetricName   string
	DimensionKey string
	SubDimension string
}

// MergeThingRollupRows is the per-Thing twin of MergeRollupRows. Identical
// merge semantics (histogram / timestamp / distinct-max / sum, driven by the
// aggregation-kind registry) keyed by (ThingID, MetricName, DimensionKey,
// SubDimension). Insertion order is preserved.
func MergeThingRollupRows(rows []ThingRollupRow) []ThingRollupRow {
	acc := make(map[thingRowKey]*ThingRollupRow, len(rows))
	var order []thingRowKey

	for _, r := range rows {
		k := thingRowKey{
			ThingID:      r.ThingID,
			MetricName:   r.MetricName,
			DimensionKey: r.DimensionKey,
			SubDimension: r.SubDimension,
		}
		existing, ok := acc[k]
		if !ok {
			clone := r
			acc[k] = &clone
			order = append(order, k)
			continue
		}
		switch {
		case IsHistogramMetric(r.MetricName):
			mergeHistogramMetadataThing(existing, r)
		case IsTimestampMetric(r.MetricName):
			mergeTimestampMetadataThing(existing, r)
		default:
			// Sum-kind adds; distinct/gauge-kind takes the max so coarse-tier
			// distinct counts are not inflated across the merge cascade.
			existing.Value = CombineValues(r.MetricName, existing.Value, r.Value)
		}
	}

	out := make([]ThingRollupRow, 0, len(order))
	for _, k := range order {
		out = append(out, *acc[k])
	}
	return out
}

func mergeHistogramMetadataThing(dst *ThingRollupRow, src ThingRollupRow) {
	hDst, err := ParseHistogramMetadata(dst.Metadata)
	if err != nil {
		dst.Metadata = src.Metadata
		return
	}
	hSrc, err := ParseHistogramMetadata(src.Metadata)
	if err != nil {
		return
	}
	merged := MergeHistograms(hDst, hSrc)
	data, err := json.Marshal(merged)
	if err != nil {
		return
	}
	dst.Metadata = data
}

func mergeTimestampMetadataThing(dst *ThingRollupRow, src ThingRollupRow) {
	var tsDst, tsSrc TimestampMeta
	if len(dst.Metadata) > 0 {
		if err := json.Unmarshal(dst.Metadata, &tsDst); err != nil {
			dst.Metadata = src.Metadata
			return
		}
	}
	if len(src.Metadata) > 0 {
		if err := json.Unmarshal(src.Metadata, &tsSrc); err != nil {
			return
		}
	}
	merged := MergeTimestampMeta(tsDst, tsSrc)
	data, err := json.Marshal(merged)
	if err != nil {
		return
	}
	dst.Metadata = data
}

// MergeRollupRows combines rows that share the same (MetricName, DimensionKey,
// SubDimension) using the per-metric strategy from the aggregation-kind
// registry:
//   - Histogram metrics: element-wise addition of bucket counts in metadata
//   - Timestamp metrics: MIN(first_seen), MAX(last_seen) in metadata
//   - Distinct-cardinality metrics (active_entities / active_organizations /
//     distinct_sources): MAX of Value — per-bucket distinct counts must not be
//     summed across the cascade, which would over-count (see CombineValues).
//   - All other metrics: simple SUM of Value (the default)
//
// Insertion order is preserved: the first occurrence of each key determines its
// position in the output slice.
func MergeRollupRows(rows []RollupRow) []RollupRow {
	acc := make(map[rowKey]*RollupRow, len(rows))
	var order []rowKey

	for _, r := range rows {
		k := rowKey{
			MetricName:   r.MetricName,
			DimensionKey: r.DimensionKey,
			SubDimension: r.SubDimension,
		}

		existing, ok := acc[k]
		if !ok {
			// First occurrence: clone the row and store it.
			clone := r
			acc[k] = &clone
			order = append(order, k)
			continue
		}

		// Merge into existing row.
		switch {
		case IsHistogramMetric(r.MetricName):
			mergeHistogramMetadata(existing, r)
		case IsTimestampMetric(r.MetricName):
			mergeTimestampMetadata(existing, r)
		default:
			// Sum-kind adds; distinct/gauge-kind takes the max so coarse-tier
			// distinct counts are not inflated across the merge cascade.
			existing.Value = CombineValues(r.MetricName, existing.Value, r.Value)
		}
	}

	out := make([]RollupRow, 0, len(order))
	for _, k := range order {
		out = append(out, *acc[k])
	}
	return out
}

// mergeHistogramMetadata parses both histograms from metadata, merges them
// element-wise, and writes the result back to dst.Metadata.
func mergeHistogramMetadata(dst *RollupRow, src RollupRow) {
	hDst, err := ParseHistogramMetadata(dst.Metadata)
	if err != nil {
		// If existing metadata is unparseable, overwrite with src.
		dst.Metadata = src.Metadata
		return
	}
	hSrc, err := ParseHistogramMetadata(src.Metadata)
	if err != nil {
		return // Skip unparseable source.
	}

	merged := MergeHistograms(hDst, hSrc)
	data, err := json.Marshal(merged)
	if err != nil {
		return
	}
	dst.Metadata = data
}

// mergeTimestampMetadata parses both timestamp metas, merges with MIN/MAX
// semantics, and writes the result back to dst.Metadata.
func mergeTimestampMetadata(dst *RollupRow, src RollupRow) {
	var tsDst, tsSrc TimestampMeta

	if len(dst.Metadata) > 0 {
		if err := json.Unmarshal(dst.Metadata, &tsDst); err != nil {
			dst.Metadata = src.Metadata
			return
		}
	}
	if len(src.Metadata) > 0 {
		if err := json.Unmarshal(src.Metadata, &tsSrc); err != nil {
			return
		}
	}

	merged := MergeTimestampMeta(tsDst, tsSrc)
	data, err := json.Marshal(merged)
	if err != nil {
		return
	}
	dst.Metadata = data
}

// ParseDimensionKey splits a "dimension=value" string into its parts. If the
// input is empty, both return values are empty strings. If there is no "="
// separator, the entire input is returned as the dimension with an empty value.
func ParseDimensionKey(dk string) (dimension, value string) {
	if dk == "" {
		return "", ""
	}
	idx := strings.IndexByte(dk, '=')
	if idx < 0 {
		return dk, ""
	}
	return dk[:idx], dk[idx+1:]
}
