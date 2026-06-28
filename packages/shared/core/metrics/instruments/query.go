package instruments

import (
	"github.com/goccy/go-json"
	"sort"
	"time"
)

// BuildResult transforms raw RollupRows into a structured MetricsResult based
// on the query parameters. This is a pure function — no DB access.
//
//   - If q.TimeSeries: builds time-series buckets in result.Series
//   - Else if q.DimensionKey != "": builds grouped aggregates in result.Groups
//   - Else: builds a flat summary in result.Summary
func BuildResult(q MetricsQuery, rows []RollupRow, granularity Granularity) *MetricsResult {
	result := &MetricsResult{
		Granularity: string(granularity),
		Source:      "rollup",
	}

	switch {
	case q.TimeSeries:
		result.Series = buildTimeSeries(rows, q.Metrics)
	case q.DimensionKey != "":
		result.Groups = buildGroups(rows, q.Metrics, q.TopN)
		result.Metadata = buildGroupMetadata(rows)
	default:
		result.Summary = buildSummary(rows, q.Metrics)
		result.Metadata = buildSummaryMetadata(rows)
	}

	return result
}

// buildSummary folds all row values by MetricName, filtered to the requested
// metrics list. Additive metrics sum; distinct-cardinality metrics take the
// max across buckets (see CombineValues) so a multi-bucket read window does not
// over-count distinct entities / orgs / sources.
func buildSummary(rows []RollupRow, metrics []string) map[string]float64 {
	allowed := makeSet(metrics)
	sums := make(map[string]float64, len(metrics))
	for _, r := range rows {
		if !allowed[r.MetricName] {
			continue
		}
		sums[r.MetricName] = CombineValues(r.MetricName, sums[r.MetricName], r.Value)
	}
	return sums
}

// buildSummaryMetadata merges histogram and timestamp metadata across all rows.
func buildSummaryMetadata(rows []RollupRow) map[string]any {
	meta := make(map[string]any)
	histograms := make(map[string]Histogram)
	timestamps := make(map[string]TimestampMeta)

	for _, r := range rows {
		if len(r.Metadata) == 0 {
			continue
		}
		switch {
		case IsHistogramMetric(r.MetricName):
			h, err := ParseHistogramMetadata(r.Metadata)
			if err != nil {
				continue
			}
			if existing, ok := histograms[r.MetricName]; ok {
				histograms[r.MetricName] = MergeHistograms(existing, h)
			} else {
				histograms[r.MetricName] = h
			}
		case IsTimestampMetric(r.MetricName):
			var ts TimestampMeta
			if err := json.Unmarshal(r.Metadata, &ts); err != nil {
				continue
			}
			if existing, ok := timestamps[r.MetricName]; ok {
				timestamps[r.MetricName] = MergeTimestampMeta(existing, ts)
			} else {
				timestamps[r.MetricName] = ts
			}
		}
	}

	for k, v := range histograms {
		meta[k] = v
	}
	for k, v := range timestamps {
		meta[k] = v
	}

	if len(meta) == 0 {
		return nil
	}
	return meta
}

// buildGroups groups rows by DimensionKey, folds values per metric per group
// (additive metrics sum; distinct-cardinality metrics take the max across the
// group's buckets — see CombineValues), and optionally truncates to topN
// entries sorted by the first metric descending. The max fold is what keeps
// per-group distinct counts (e.g. the Top Destinations DeviceCount) from being
// inflated by the number of buckets in the read window.
func buildGroups(rows []RollupRow, metrics []string, topN int) []MetricsGroup {
	allowed := makeSet(metrics)

	// Accumulate per dimension key.
	type groupAcc struct {
		values map[string]float64
	}
	grouped := make(map[string]*groupAcc)
	var order []string // track insertion order for deterministic output

	for _, r := range rows {
		if !allowed[r.MetricName] {
			continue
		}
		acc, ok := grouped[r.DimensionKey]
		if !ok {
			acc = &groupAcc{values: make(map[string]float64, len(metrics))}
			grouped[r.DimensionKey] = acc
			order = append(order, r.DimensionKey)
		}
		acc.values[r.MetricName] = CombineValues(r.MetricName, acc.values[r.MetricName], r.Value)
	}

	groups := make([]MetricsGroup, 0, len(grouped))
	for _, dk := range order {
		acc := grouped[dk]
		groups = append(groups, MetricsGroup{
			DimensionKey: dk,
			Values:       acc.values,
		})
	}

	// Sort by first metric value descending for TopN selection.
	if len(metrics) > 0 {
		sortMetric := metrics[0]
		sort.SliceStable(groups, func(i, j int) bool {
			return groups[i].Values[sortMetric] > groups[j].Values[sortMetric]
		})
	}

	if topN > 0 && topN < len(groups) {
		groups = groups[:topN]
	}

	return groups
}

// buildGroupMetadata merges histogram and timestamp metadata per
// "dimensionKey:metricName" composite key.
func buildGroupMetadata(rows []RollupRow) map[string]any {
	meta := make(map[string]any)
	histograms := make(map[string]Histogram)
	timestamps := make(map[string]TimestampMeta)

	for _, r := range rows {
		if len(r.Metadata) == 0 {
			continue
		}
		key := r.DimensionKey + ":" + r.MetricName
		switch {
		case IsHistogramMetric(r.MetricName):
			h, err := ParseHistogramMetadata(r.Metadata)
			if err != nil {
				continue
			}
			if existing, ok := histograms[key]; ok {
				histograms[key] = MergeHistograms(existing, h)
			} else {
				histograms[key] = h
			}
		case IsTimestampMetric(r.MetricName):
			var ts TimestampMeta
			if err := json.Unmarshal(r.Metadata, &ts); err != nil {
				continue
			}
			if existing, ok := timestamps[key]; ok {
				timestamps[key] = MergeTimestampMeta(existing, ts)
			} else {
				timestamps[key] = ts
			}
		}
	}

	for k, v := range histograms {
		meta[k] = v
	}
	for k, v := range timestamps {
		meta[k] = v
	}

	if len(meta) == 0 {
		return nil
	}
	return meta
}

// buildTimeSeries groups rows by BucketStart, folds values per metric per
// bucket (additive metrics sum; distinct-cardinality metrics take the max
// across the rows that fall in the same bucket — see CombineValues), and
// returns chronologically ordered buckets.
func buildTimeSeries(rows []RollupRow, metrics []string) []MetricsBucket {
	allowed := makeSet(metrics)

	type bucketAcc struct {
		start  time.Time
		values map[string]float64
	}

	buckets := make(map[string]*bucketAcc) // keyed by RFC3339 string
	var order []string

	for _, r := range rows {
		if !allowed[r.MetricName] {
			continue
		}
		key := r.BucketStart.UTC().Format(time.RFC3339)
		acc, ok := buckets[key]
		if !ok {
			acc = &bucketAcc{
				start:  r.BucketStart,
				values: make(map[string]float64, len(metrics)),
			}
			buckets[key] = acc
			order = append(order, key)
		}
		acc.values[r.MetricName] = CombineValues(r.MetricName, acc.values[r.MetricName], r.Value)
	}

	result := make([]MetricsBucket, 0, len(buckets))
	for _, key := range order {
		acc := buckets[key]
		result = append(result, MetricsBucket{
			BucketStart: acc.start,
			Values:      acc.values,
		})
	}

	// Sort chronologically.
	sort.Slice(result, func(i, j int) bool {
		return result[i].BucketStart.Before(result[j].BucketStart)
	})

	return result
}

// makeSet creates a string set from a slice for O(1) lookup.
func makeSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}
