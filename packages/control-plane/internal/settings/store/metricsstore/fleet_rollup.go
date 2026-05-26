package metricsstore

import "context"

// MetricRollupBucket holds a single metric rollup bucket for
// the fleet-analytics trends endpoint.
type MetricRollupBucket struct {
	BucketStart any     `json:"bucketStart"`
	Dimensions  string  `json:"dimensions"`
	Value       float64 `json:"value"`
}

// TopDestination holds a top destination host result for
// the fleet-analytics top-destinations endpoint.
type TopDestination struct {
	DestHost    string `json:"destHost"`
	EventCount  int    `json:"eventCount"`
	DeviceCount int    `json:"deviceCount"`
}

// ListMetricRollupBuckets returns hourly rollup buckets for a given
// metric from the metric_rollup_1h table. Used by fleet analytics
// trend endpoints. Limit controls the maximum number of rows returned.
func (s *Store) ListMetricRollupBuckets(ctx context.Context, metricName string, limit int) ([]MetricRollupBucket, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT "bucketStart", "dimensionKey", "value" FROM metric_rollup_1h
		WHERE "metricName" = $1 ORDER BY "bucketStart" DESC LIMIT $2
	`, metricName, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := []MetricRollupBucket{}
	for rows.Next() {
		var b MetricRollupBucket
		if err := rows.Scan(&b.BucketStart, &b.Dimensions, &b.Value); err != nil {
			return nil, err
		}
		result = append(result, b)
	}
	return result, rows.Err()
}
