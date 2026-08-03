package core

import (
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

func TestStrategyNode_LatencyHydratesFromTargetsKey(t *testing.T) {
	// The admin UI authors latency targets under the generic "targets" key; the
	// unmarshal shim must hydrate LatencyTargets from it.
	var n StrategyNode
	if err := json.Unmarshal([]byte(`{"type":"latency","targets":[
		{"providerId":"pa","modelId":"m1"},
		{"providerId":"pb","modelId":"m2"}]}`), &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(n.LatencyTargets) != 2 {
		t.Fatalf("expected 2 LatencyTargets hydrated from targets, got %d", len(n.LatencyTargets))
	}
	if n.LatencyTargets[0].ProviderID != "pa" || n.LatencyTargets[1].ModelID != "m2" {
		t.Fatalf("wrong hydrated targets: %+v", n.LatencyTargets)
	}
}

func TestStrategyNode_LatencyExplicitKeyHonored(t *testing.T) {
	// An explicit latencyTargets key wins and is not overwritten by the targets shim.
	var n StrategyNode
	if err := json.Unmarshal([]byte(`{"type":"latency",
		"latencyTargets":[{"providerId":"explicit","modelId":"m"}],
		"targets":[{"providerId":"ignored","modelId":"m"}]}`), &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(n.LatencyTargets) != 1 || n.LatencyTargets[0].ProviderID != "explicit" {
		t.Fatalf("explicit latencyTargets should win, got %+v", n.LatencyTargets)
	}
}

func TestStrategyNode_LatencyCapsFanOut(t *testing.T) {
	// A list longer than MaxLatencyTargets is trimmed at unmarshal.
	var sb strings.Builder
	sb.WriteString(`{"type":"latency","targets":[`)
	for i := range MaxLatencyTargets + 10 {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"providerId":"p` + strconv.Itoa(i) + `","modelId":"m"}`)
	}
	sb.WriteString(`]}`)

	var n StrategyNode
	if err := json.Unmarshal([]byte(sb.String()), &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(n.LatencyTargets) != MaxLatencyTargets {
		t.Fatalf("expected fan-out capped at %d, got %d", MaxLatencyTargets, len(n.LatencyTargets))
	}
}

func TestStrategyNode_ABSplitStillHydrates(t *testing.T) {
	// Regression: the latency branch must not disturb the ab_split targets shim.
	var n StrategyNode
	if err := json.Unmarshal([]byte(`{"type":"ab_split","targets":[
		{"providerId":"pa","modelId":"m","weight":70}]}`), &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(n.ABTargets) != 1 || n.ABTargets[0].Weight != 70 {
		t.Fatalf("ab_split hydrate regressed: %+v", n.ABTargets)
	}
}
