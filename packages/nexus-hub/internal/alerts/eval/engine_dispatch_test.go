package alerteval

import "testing"

// TestDispatchTable_RoutesBySourceMask verifies the H7 lock-free dispatch table
// routes each event to exactly the aggregators whose declared Sources() include
// the event's source — and to no others.
func TestDispatchTable_RoutesBySourceMask(t *testing.T) {
	e := newEngineForTest(t, &fakeRuleLister{}, &fakeAlertSink{}, &stubMQConsumer{})
	aiOnly := &fireAggregator{id: "ai", sources: []EventSource{SourceAITraffic}}
	agentOnly := &fireAggregator{id: "agent", sources: []EventSource{SourceAgent}}
	multi := &fireAggregator{id: "multi", sources: []EventSource{SourceAITraffic, SourceCompliance}}
	none := &fireAggregator{id: "none", sources: nil} // declares no source → matches nothing
	for _, a := range []*fireAggregator{aiOnly, agentOnly, multi, none} {
		e.Register(a)
	}

	evt := &Event{Kind: EventTraffic}
	for _, src := range []EventSource{SourceAITraffic, SourceAgent, SourceCompliance, SourceAdminAudit} {
		e.dispatchEvent(src, evt)
	}

	for _, c := range []struct {
		agg  *fireAggregator
		want int
	}{
		{aiOnly, 1},    // ai-traffic only
		{agentOnly, 1}, // agent only
		{multi, 2},     // ai-traffic + compliance
		{none, 0},      // no declared source
	} {
		if got := c.agg.eventCount(); got != c.want {
			t.Errorf("%s received %d events, want %d", c.agg.id, got, c.want)
		}
	}
}

// TestSourceMask_ParityWithAggMatchesSource locks the bitmask routing as exactly
// equivalent to the pre-H7 aggMatchesSource for every (aggregator, source) combo,
// including an unknown source (must match nothing) and an aggregator with no
// declared sources.
func TestSourceMask_ParityWithAggMatchesSource(t *testing.T) {
	allSrc := []EventSource{SourceAITraffic, SourceCompliance, SourceAgent, SourceAdminAudit, EventSource("unknown")}
	aggs := []Aggregator{
		&fireAggregator{id: "a", sources: []EventSource{SourceAITraffic}},
		&fireAggregator{id: "b", sources: []EventSource{SourceAITraffic, SourceCompliance}},
		&fireAggregator{id: "c", sources: []EventSource{SourceAgent, SourceAdminAudit}},
		&fireAggregator{id: "d", sources: nil},
		&fireAggregator{id: "e", sources: []EventSource{SourceAITraffic, SourceCompliance, SourceAgent, SourceAdminAudit}},
	}
	for _, agg := range aggs {
		var mask uint8
		for _, s := range agg.Sources() {
			mask |= sourceBit(s)
		}
		for _, src := range allSrc {
			byMask := mask&sourceBit(src) != 0
			byOld := aggMatchesSource(agg, src)
			if byMask != byOld {
				t.Errorf("agg %s src %q: bitmask=%v aggMatchesSource=%v (must match)", agg.RuleID(), src, byMask, byOld)
			}
		}
	}
}
