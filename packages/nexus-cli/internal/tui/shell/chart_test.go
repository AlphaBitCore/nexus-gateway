package shell

import (
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
)

func TestSparklineChart(t *testing.T) {
	series := []core.SparklineBucket{
		{Values: map[string]float64{"request_count": 5}},
		{Values: map[string]float64{"request_count": 20}},
		{Values: map[string]float64{"request_count": 12}},
	}
	if got := kit.SparklineChart(series, "request_count", 30, 4); got == "" {
		t.Fatal("a non-empty series should render a chart")
	}
	// guards: empty series / no room → ""
	if kit.SparklineChart(nil, "request_count", 30, 4) != "" {
		t.Fatal("empty series → no chart")
	}
	if kit.SparklineChart(series, "request_count", 2, 0) != "" {
		t.Fatal("no room → no chart")
	}
}

func TestTile(t *testing.T) {
	out := kit.Tile("Requests", "42")
	if !strings.Contains(out, "Requests") || !strings.Contains(out, "42") {
		t.Fatalf("tile should render the label over the value, got:\n%s", out)
	}
}
