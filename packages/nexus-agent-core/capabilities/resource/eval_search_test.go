package resource

import (
	"fmt"
	"testing"
)

// eval_search_test.go is the deterministic half of the search-quality eval
// (design: docs/superpowers/specs/2026-06-05-resource-catalog-ai-first-design.md §3.5).
// It scores Search() against a golden set of real operator questions and pins
// the measured hit rates as regression floors, so any change to the scorer or
// the catalog that degrades retrieval fails here instead of in an agent session.
//
// Golden-set ownership: every NEW kind added to the catalog adds at least one
// question here in the same PR. Questions are phrased the way an operator
// actually asks, in three classes:
//   - structural: the words appear in the kind/operationId/path
//   - summary:    the words appear only in the operation's OpenAPI summary
//   - purpose:    neither — the question states intent in the operator's words
//
// expected lists every operationId that correctly answers the question (some
// questions have two legitimate targets across kinds); a hit on any counts.
// The floors below are the values measured on the CURRENT implementation
// (baseline, 2026-06-05) — raise them when retrieval improves, never lower
// them without explicit design-doc justification.

type goldenQuestion struct {
	query    string
	class    string   // structural | summary | purpose
	expected []string // operationIds; any hit counts
}

var searchGoldenSet = []goldenQuestion{
	// --- structural: words present in kind/opId/path ---
	{"simulate an iam decision", "structural", []string{"simulateIAM"}},
	{"list routing rules", "structural", []string{"listRoutingRules"}},
	{"create a virtual key", "structural", []string{"createVirtualKey"}},
	{"approve a pending virtual key", "structural", []string{"approveVirtualKey"}},
	{"download the proxy ca certificate", "structural", []string{"setupGetCACert"}},
	{"trigger a scheduled job now", "structural", []string{"jobsTrigger"}},
	{"import a rule pack from yaml", "structural", []string{"import"}},
	{"rotate a device certificate", "structural", []string{"rotateAgentCert"}},
	{"delete a quota override", "structural", []string{"deleteQuotaOverride"}},
	{"list dead letter queue entries", "structural", []string{"listDLQ"}},

	// --- summary: words only in the OpenAPI summary ---
	{"which nodes are out of sync", "summary", []string{"configSyncOutOfSync"}},
	{"force a device config re-push", "summary", []string{"forceRefreshAgentDevice"}},
	{"send a test alert through a channel", "summary", []string{"testAlertChannel"}},
	{"reset a credential circuit breaker", "summary", []string{"circuitReset"}},
	{"dry-run a prompt against freshness rules", "summary", []string{"testTimeSensitivePattern"}},
	{"preview smart group membership", "summary", []string{"previewMembership"}},
	{"toggle the data-plane kill switch", "summary", []string{"post"}},
	{"rolling 30-day cost summary", "summary", []string{"analyticsCostSummary"}},
	{"per-phase latency percentiles", "summary", []string{"analyticsLatencyPhases"}},
	{"get the organization hierarchy", "summary", []string{"organizationTree"}},
	{"stop an in-flight assistant turn", "summary", []string{"interruptSession"}},

	// --- purpose: intent-phrased; neither path nor summary words guaranteed ---
	{"force logout all sessions of a user", "purpose", []string{"deleteAuthSessions"}},
	{"is this provider api key still valid", "purpose", []string{"probeCredential", "providerTest"}},
	{"why did my request fall back to another provider", "purpose", []string{"analyticsRoutingFallbacks"}},
	{"retry a stuck message", "purpose", []string{"retryDLQ"}},
	{"kick a device out of the fleet", "purpose", []string{"unenrollDevice"}},
	{"what admin actions am i allowed to perform", "purpose", []string{"getMePermissions"}},
	{"who changed this configuration", "purpose", []string{"listAdminAuditLogs", "configSyncHistory"}},
	{"export compliance events as csv", "purpose", []string{"complianceOverviewExport", "proxyComplianceExport"}},
	{"how healthy is the agent fleet", "purpose", []string{"agentFleetHealth", "fleetAnalyticsSummary"}},
	{"cache hit rate and savings", "purpose", []string{"cacheStats", "analyticsCacheROI"}},
}

// Baseline floors measured on the pre-card implementation (2026-06-05).
// CI fails if retrieval drops below these; raise after verified improvements.
const (
	evalFloorTop1  = 80 // baseline 2026-06-05: structural 100 / summary 90 / purpose 50
	evalFloorTop5  = 86 // rank 6-8 had zero hits on the baseline set → card count K=5
	evalFloorTop20 = 93 // the thin tail's recall margin over the cards (+7pp)
)

// evalHit reports the best rank (1-based) of any expected operationId in ops,
// or 0 when none is present.
func evalHit(q goldenQuestion, ops []Operation) int {
	for i, op := range ops {
		for _, want := range q.expected {
			if op.OperationID == want {
				return i + 1
			}
		}
	}
	return 0
}

// TestSearchGoldenEval scores Search() over the golden set and enforces the
// baseline floors. The per-class breakdown in the log is the diagnostic view:
// "purpose" questions are the ones the summary-blind result shape loses.
func TestSearchGoldenEval(t *testing.T) {
	type tally struct{ n, top1, top5, top20 int }
	totals := tally{}
	perClass := map[string]*tally{}
	for _, q := range searchGoldenSet {
		c, ok := perClass[q.class]
		if !ok {
			c = &tally{}
			perClass[q.class] = c
		}
		rank := evalHit(q, Search(q.query, 20))
		totals.n++
		c.n++
		switch {
		case rank == 0:
			t.Logf("MISS  [%s] %q → wanted %v", q.class, q.query, q.expected)
		case rank == 1:
			totals.top1++
			totals.top5++
			totals.top20++
			c.top1++
			c.top5++
			c.top20++
		case rank <= 5:
			totals.top5++
			totals.top20++
			c.top5++
			c.top20++
			t.Logf("rank%d [%s] %q", rank, q.class, q.query)
		default:
			totals.top20++
			c.top20++
			t.Logf("rank%d [%s] %q", rank, q.class, q.query)
		}
	}
	pct := func(hit, n int) int {
		if n == 0 {
			return 0
		}
		return hit * 100 / n
	}
	report := fmt.Sprintf("golden eval: top-1 %d%% top-5 %d%% top-20 %d%% (n=%d)",
		pct(totals.top1, totals.n), pct(totals.top5, totals.n), pct(totals.top20, totals.n), totals.n)
	for _, class := range []string{"structural", "summary", "purpose"} {
		c := perClass[class]
		report += fmt.Sprintf("\n  %-10s top-1 %3d%% top-5 %3d%% top-20 %3d%% (n=%d)",
			class, pct(c.top1, c.n), pct(c.top5, c.n), pct(c.top20, c.n), c.n)
	}
	t.Log(report)

	if got := pct(totals.top1, totals.n); got < evalFloorTop1 {
		t.Errorf("top-1 hit rate %d%% fell below the pinned baseline %d%%", got, evalFloorTop1)
	}
	if got := pct(totals.top5, totals.n); got < evalFloorTop5 {
		t.Errorf("top-5 hit rate %d%% fell below the pinned baseline %d%%", got, evalFloorTop5)
	}
	if got := pct(totals.top20, totals.n); got < evalFloorTop20 {
		t.Errorf("top-20 hit rate %d%% fell below the pinned baseline %d%%", got, evalFloorTop20)
	}
}
