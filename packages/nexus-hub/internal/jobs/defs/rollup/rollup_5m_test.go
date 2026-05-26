package rollup

import (
	"testing"
	"time"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

func TestRollup5m_Identity(t *testing.T) {
	j := NewRollup5m(nil, time.Minute, testLogger(), false)
	if j.ID() != "rollup-5m" {
		t.Errorf("ID = %q, want rollup-5m", j.ID())
	}
	if j.Name() == "" {
		t.Error("Name must not be empty")
	}
	if j.Description() == "" {
		t.Error("Description must not be empty")
	}
	if j.Interval() != time.Minute {
		t.Errorf("Interval = %v, want 1m", j.Interval())
	}
}

func TestRollup5m_IntervalDefault(t *testing.T) {
	j := NewRollup5m(nil, 0, testLogger(), false)
	if j.Interval() != time.Minute {
		t.Errorf("Interval = %v, want 1m default", j.Interval())
	}
}

func TestBuildEventDims(t *testing.T) {
	tests := []struct {
		name         string
		in           []string // provider, model, entityID, entityType, orgID, routedProvider, routingRuleID, targetHost, virtualKeySlug, projectID, hookDecision
		wantPairs    map[string]string
		wantAbsent   []string // dim names that must NOT appear
		wantGlobalOk bool
	}{
		{
			name: "vk-authenticated user request with project context",
			in:   []string{"openai", "gpt-4", "nexus-user-sso", "user", "org-acme", "openai", "rr-1", "api.openai.com", "vk-eng-openai", "proj-platform", ""},
			// `provider` (the requested provider) is no longer emitted —
			// OpenAI-style traffic doesn't pin a provider, and the
			// rollup writer dropped the empty-value dim. routed_provider
			// is the canonical provider dimension.
			wantPairs:    map[string]string{"model": "gpt-4", "entity": "nexus-user-sso", "user": "nexus-user-sso", "organization": "org-acme", "routed_provider": "openai", "routing_rule": "rr-1", "target_host": "api.openai.com", "virtual_key": "vk-eng-openai", "project": "proj-platform"},
			wantAbsent:   []string{"provider", "hook_decision"},
			wantGlobalOk: true,
		},
		{
			name:         "agent device request — no project/vk",
			in:           []string{"", "", "agent-dev-mac-01", "device", "", "", "", "internal.corp.local", "", "", ""},
			wantPairs:    map[string]string{"entity": "agent-dev-mac-01", "device": "agent-dev-mac-01", "target_host": "internal.corp.local"},
			wantAbsent:   []string{"provider", "model", "user", "project", "virtual_key", "hook_decision"},
			wantGlobalOk: true,
		},
		{
			name:         "project comes from identity even when entity_type is user",
			in:           []string{"anthropic", "", "nexus-user-sso", "user", "", "", "", "", "", "proj-ml", ""},
			wantPairs:    map[string]string{"user": "nexus-user-sso", "project": "proj-ml"},
			wantAbsent:   []string{"provider"},
			wantGlobalOk: true,
		},
		{
			name:         "entity_type=project also emits project dim (backwards compat)",
			in:           []string{"", "", "proj-legacy", "project", "", "", "", "", "", "", ""},
			wantPairs:    map[string]string{"entity": "proj-legacy", "project": "proj-legacy"},
			wantGlobalOk: true,
		},
		{
			name:         "only global when all inputs empty",
			in:           []string{"", "", "", "", "", "", "", "", "", "", ""},
			wantPairs:    map[string]string{},
			wantAbsent:   []string{"provider", "model", "entity", "user", "device", "project", "organization", "virtual_key", "hook_decision"},
			wantGlobalOk: true,
		},
		{
			name:         "hook decision deny emits hook_decision dim",
			in:           []string{"", "", "", "", "", "", "", "api.openai.com", "", "", "deny"},
			wantPairs:    map[string]string{"target_host": "api.openai.com", "hook_decision": "deny"},
			wantGlobalOk: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildEventDims(tc.in[0], tc.in[1], tc.in[2], tc.in[3], tc.in[4], tc.in[5], tc.in[6], tc.in[7], tc.in[8], tc.in[9], tc.in[10])
			gotMap := map[string]string{}
			hasGlobal := false
			for _, d := range got {
				if d.name == "" && d.value == "" {
					hasGlobal = true
					continue
				}
				gotMap[d.name] = d.value
			}
			if hasGlobal != tc.wantGlobalOk {
				t.Errorf("global dim present = %v, want %v", hasGlobal, tc.wantGlobalOk)
			}
			for k, v := range tc.wantPairs {
				if gotMap[k] != v {
					t.Errorf("dim %q = %q, want %q", k, gotMap[k], v)
				}
			}
			for _, k := range tc.wantAbsent {
				if _, present := gotMap[k]; present {
					t.Errorf("dim %q must be absent, got %q", k, gotMap[k])
				}
			}
		})
	}
}

// TestRollup5m_ColdStartWatermark_BackfillsHistoricalTraffic pins the
// contract that on first run (zero watermark), the aggregator rewinds past
// the default lookback if traffic_event already holds older events. Without
// this, seeded or imported historical traffic never enters the rollup and
// every analytics groupBy that depends on pre-boot data returns empty.
func TestRollup5m_ColdStartWatermark_BackfillsHistoricalTraffic(t *testing.T) {
	now := time.Date(2026, 4, 21, 14, 0, 0, 0, time.UTC)
	lookback := rollup5mInitLookback
	bucket := bucketDuration5m

	tests := []struct {
		name           string
		earliestSource time.Time
		haveSource     bool
		want           time.Time
	}{
		{
			name:       "empty traffic_event — default lookback",
			haveSource: false,
			want:       time.Date(2026, 4, 21, 13, 0, 0, 0, time.UTC),
		},
		{
			name:           "traffic inside lookback — default lookback wins",
			earliestSource: time.Date(2026, 4, 21, 13, 22, 0, 0, time.UTC),
			haveSource:     true,
			want:           time.Date(2026, 4, 21, 13, 0, 0, 0, time.UTC),
		},
		{
			name:           "historical traffic (days old) — rewinds to one bucket before earliest",
			earliestSource: time.Date(2026, 4, 18, 14, 54, 47, 0, time.UTC),
			haveSource:     true,
			want:           time.Date(2026, 4, 18, 14, 45, 0, 0, time.UTC),
		},
		{
			name:           "earliest traffic exactly on lookback boundary — rewinds one bucket earlier",
			earliestSource: time.Date(2026, 4, 21, 13, 0, 0, 0, time.UTC),
			haveSource:     true,
			want:           time.Date(2026, 4, 21, 12, 55, 0, 0, time.UTC),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pickColdStartWatermark(now, lookback, bucket, tc.earliestSource, tc.haveSource)
			if !got.Equal(tc.want) {
				t.Errorf("= %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRollup5m_DeduplicateRows_KeepsLast(t *testing.T) {
	// Two rows with the same (metric, dim, subDim) must collapse into one,
	// and the last one wins — matches CP semantics.
	rows := []metrics.RollupRow{
		{MetricName: "request_count", DimensionKey: "provider=openai", SubDimension: "source=vk", Value: 5},
		{MetricName: "request_count", DimensionKey: "provider=openai", SubDimension: "source=vk", Value: 7},
		{MetricName: "request_count", DimensionKey: "provider=openai", SubDimension: "source=proxy", Value: 3},
	}
	got := deduplicateRows5m(rows)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (collapsed first two)", len(got))
	}
	var vkValue float64
	for _, r := range got {
		if r.DimensionKey == "provider=openai" && r.SubDimension == "source=vk" {
			vkValue = r.Value
		}
	}
	if vkValue != 7 {
		t.Errorf("vk row value = %v, want 7 (last wins)", vkValue)
	}
}
