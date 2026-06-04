package kit

import (
	"fmt"

	"github.com/NimbleMarkets/ntcharts/sparkline"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// SparklineChart renders a braille sparkline of one metric across the series
// buckets. Returns "" when there is nothing to draw or no room.
func SparklineChart(series []core.SparklineBucket, metric string, width, height int) string {
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

// Tile renders one bordered "card" for a big-number metric (label over value).
// Shared by the SLO and Compliance summary rows.
func Tile(label, value string) string {
	inner := styles.TileLabel.Render(label) + "\n" + styles.TileValue.Render(value)
	return styles.Tile.Render(inner)
}

// DetailRow renders one "label  value" line for a row-drill detail drawer. The
// label is muted and left-padded so the values line up into a column. Shared by
// every list view's detail drawer (Nodes / Alerts / Jobs / Compliance / Sync /
// Models).
func DetailRow(label, value string) string {
	return styles.TileLabel.Render(fmt.Sprintf("  %-16s ", label)) + value
}
