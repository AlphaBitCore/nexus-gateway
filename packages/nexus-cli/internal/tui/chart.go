package tui

import (
	"github.com/NimbleMarkets/ntcharts/sparkline"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
)

// chartMetric is one selectable Overview chart series.
type chartMetric struct {
	key   string
	label string
}

// chartMetrics are the Overview time-series options (c cycles them). Keys are
// the snake_case metric instrument names the sparkline series uses.
var chartMetrics = []chartMetric{
	{key: core.MetricRequestCount, label: "requests"},
	{key: core.MetricEstimatedCostUSD, label: "cost $"},
	{key: "latency_us_sum", label: "latency"},
}

// sparklineChart renders a braille sparkline of one metric across the series
// buckets. Returns "" when there is nothing to draw or no room.
func sparklineChart(series []core.SparklineBucket, metric string, width, height int) string {
	if len(series) == 0 || width < 4 || height < 1 {
		return ""
	}
	vals := make([]float64, 0, len(series))
	for _, b := range series {
		vals = append(vals, b.Values[metric])
	}
	sl := sparkline.New(width, height)
	sl.PushAll(vals)
	sl.DrawBraille()
	return sl.View()
}
