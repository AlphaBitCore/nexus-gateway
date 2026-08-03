package rollup

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/pashagolub/pgxmock/v4"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

// classKey builds the accumulator dimension key for an expected class.
func classKey(code, rng, provider, model string) string {
	return metrics.BuildDimensionKey(metrics.DimensionErrorClass,
		metrics.BuildErrorClassValue(metrics.ErrorClass{ErrorCode: code, StatusRange: rng, Provider: provider, Model: model}))
}

// TestErrorClassAcc_ObserveAndFold verifies the business shape of the emitted
// series: one count row per (class, source) with the row count as value, and
// one seen row carrying exact MIN(first)/MAX(last) timestamps.
func TestErrorClassAcc_ObserveAndFold(t *testing.T) {
	acc := newErrorClassAcc()
	t1 := time.Date(2026, 7, 17, 10, 0, 10, 0, time.UTC)
	t2 := t1.Add(30 * time.Second)

	// Two rows of the same class (4xx, classified, routed names) at t1 and t2.
	acc.observe(intPtr(400), nil, strPtr("invalid_request"),
		strPtr("openai"), strPtr("ignored-requested"), strPtr("gpt-4o-mini"), strPtr("ignored"),
		"source=vk", t1)
	acc.observe(intPtr(429), nil, strPtr("invalid_request"),
		strPtr("openai"), nil, strPtr("gpt-4o-mini"), nil,
		"source=vk", t2)
	// One unclassified 5xx with no names at all (early rejection shape).
	acc.observe(intPtr(502), nil, nil, nil, nil, nil, nil, "source=proxy", t1)

	accValues := map[accKey5m]float64{}
	accTimestamp := map[accKey5m]metrics.TimestampMeta{}
	acc.foldInto(testLogger(), accValues, accTimestamp)

	wantClass := classKey("invalid_request", "4xx", "openai", "gpt-4o-mini")
	if got := accValues[accKey5m{metrics.MetricTrafficErrorClassCount, wantClass, "source=vk"}]; got != 2 {
		t.Errorf("classified count = %v, want 2", got)
	}
	wantBare := classKey("", "5xx", "", "")
	if got := accValues[accKey5m{metrics.MetricTrafficErrorClassCount, wantBare, "source=proxy"}]; got != 1 {
		t.Errorf("unclassified count = %v, want 1", got)
	}

	seen := accTimestamp[accKey5m{metrics.MetricTrafficErrorClassSeen, wantClass, "source=vk"}]
	if seen.FirstSeen != t1.Format(time.RFC3339) || seen.LastSeen != t2.Format(time.RFC3339) {
		t.Errorf("seen = %+v, want first=%s last=%s", seen, t1.Format(time.RFC3339), t2.Format(time.RFC3339))
	}
}

// TestErrorClassAcc_SkipsSuccessAndInternal pins the row filter parity with
// the direct error-governance query: success rows and internal operational
// traffic must not enter the series.
func TestErrorClassAcc_SkipsSuccessAndInternal(t *testing.T) {
	acc := newErrorClassAcc()
	ts := time.Now().UTC()
	acc.observe(intPtr(200), nil, nil, nil, nil, nil, nil, "source=vk", ts)
	acc.observe(nil, nil, nil, nil, nil, nil, nil, "source=vk", ts) // NULL status
	acc.observe(intPtr(500), strPtr("ai-guard-judge"), strPtr("PROVIDER_ERROR"),
		strPtr("openai"), nil, strPtr("gpt-4o"), nil, "source=vk", ts)

	if len(acc.counts) != 0 || len(acc.seen) != 0 {
		t.Fatalf("expected empty accumulators, got counts=%d seen=%d", len(acc.counts), len(acc.seen))
	}
}

// TestErrorClassAcc_CoalesceNames pins SQL COALESCE parity: NULL falls
// through to the requested name, but an EMPTY non-NULL routed name is kept —
// the direct path's COALESCE(routed, requested, ”) behaves exactly so, and
// a divergence would make the two paths bucket the same event differently.
func TestErrorClassAcc_CoalesceNames(t *testing.T) {
	acc := newErrorClassAcc()
	ts := time.Now().UTC()

	// routed NULL → requested name used.
	acc.observe(intPtr(404), nil, strPtr("ROUTING_NO_MATCH"), nil, strPtr("req-prov"), nil, strPtr("auto"), "source=vk", ts)
	// routed EMPTY (non-NULL) → empty kept, requested NOT consulted.
	acc.observe(intPtr(404), nil, strPtr("ROUTING_NO_MATCH"), strPtr(""), strPtr("req-prov"), strPtr(""), strPtr("auto"), "source=vk", ts)

	k1 := accKey5m{metrics.MetricTrafficErrorClassCount, classKey("ROUTING_NO_MATCH", "4xx", "req-prov", "auto"), "source=vk"}
	k2 := accKey5m{metrics.MetricTrafficErrorClassCount, classKey("ROUTING_NO_MATCH", "4xx", "", ""), "source=vk"}
	if acc.counts[k1] != 1 || acc.counts[k2] != 1 {
		t.Errorf("coalesce mismatch: %v", acc.counts)
	}
}

// TestErrorClassAcc_CapFoldsOverflowDeterministically drives more distinct
// classes than the cap and asserts: survivors are exactly the top classes by
// count, folded classes merge into the per-status-range overflow row, the
// TOTAL count is preserved, and a re-run over the same input reproduces
// byte-identical keys (idempotent bucket rewrite).
func TestErrorClassAcc_CapFoldsOverflowDeterministically(t *testing.T) {
	build := func() (map[accKey5m]float64, map[accKey5m]metrics.TimestampMeta) {
		acc := newErrorClassAcc()
		ts := time.Now().UTC()
		// errorClassBucketCap high-count classes (count 2 each) + 10 singletons.
		for i := range errorClassBucketCap {
			model := fmt.Sprintf("model-%04d", i)
			acc.observe(intPtr(400), nil, strPtr("invalid_request"), strPtr("p"), nil, strPtr(model), nil, "source=vk", ts)
			acc.observe(intPtr(400), nil, strPtr("invalid_request"), strPtr("p"), nil, strPtr(model), nil, "source=vk", ts)
		}
		for i := range 10 {
			model := fmt.Sprintf("spray-%04d", i)
			acc.observe(intPtr(500), nil, strPtr("PROVIDER_ERROR"), strPtr("p"), nil, strPtr(model), nil, "source=vk", ts)
		}
		accValues := map[accKey5m]float64{}
		accTimestamp := map[accKey5m]metrics.TimestampMeta{}
		acc.foldInto(testLogger(), accValues, accTimestamp)
		return accValues, accTimestamp
	}

	v1, ts1 := build()
	v2, _ := build()

	var total float64
	overflowKey := classKey(metrics.ErrorClassOverflowCode, "5xx", "", "")
	var overflowCount float64
	distinctClasses := map[string]struct{}{}
	for k, v := range v1 {
		if k.metricName != metrics.MetricTrafficErrorClassCount {
			continue
		}
		total += v
		distinctClasses[k.dimensionKey] = struct{}{}
		if k.dimensionKey == overflowKey {
			overflowCount = v
		}
	}
	wantTotal := float64(errorClassBucketCap*2 + 10)
	if total != wantTotal {
		t.Errorf("total count = %v, want %v (counts must survive folding)", total, wantTotal)
	}
	// The 10 singleton 5xx classes rank below every count-2 class → all fold.
	if overflowCount != 10 {
		t.Errorf("overflow count = %v, want 10", overflowCount)
	}
	if len(distinctClasses) != errorClassBucketCap+1 {
		t.Errorf("distinct classes = %d, want cap+overflow = %d", len(distinctClasses), errorClassBucketCap+1)
	}
	for k := range distinctClasses {
		if strings.Contains(k, "spray-") {
			t.Errorf("sprayed singleton class %q survived the cap", k)
		}
	}
	// Overflow seen metadata must exist (folded classes carry their timestamps).
	if _, ok := ts1[accKey5m{metrics.MetricTrafficErrorClassSeen, overflowKey, "source=vk"}]; !ok {
		t.Error("overflow seen row missing")
	}

	// Determinism: identical input → identical map keys and values.
	if len(v1) != len(v2) {
		t.Fatalf("re-run produced different row count: %d vs %d", len(v1), len(v2))
	}
	for k, v := range v1 {
		if v2[k] != v {
			t.Errorf("re-run diverged at %+v: %v vs %v", k, v, v2[k])
		}
	}
}

// TestRollup5m_AggregateTrafficEvents_EmitsErrorClass drives the full
// aggregator with one failed row and asserts the error-class series lands in
// the assembled rollup rows — proving the SELECT/scan wiring, not just the
// accumulator.
func TestRollup5m_AggregateTrafficEvents_EmitsErrorClass(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := time.Now().UTC().Truncate(5 * time.Minute).Add(-10 * time.Minute)
	ts := bucket.Add(time.Minute)

	row := oneTrafficEventRow(ts)
	// status 429 + error_code + routed names via the error-class tail columns.
	sc := 429
	row[10] = &sc // status_code
	code := "RATE_LIMITED"
	row[26] = &code // error_code
	tail := len(row) - len(trafficEventErrorClassTail)
	row[tail+1] = strPtr("openai")      // routed_provider_name
	row[tail+3] = strPtr("gpt-4o-mini") // routed_model_name

	mock.ExpectBegin()
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(trafficEventCols).AddRow(row...))

	tx, err := mock.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	j := NewRollup5m(nil, time.Minute, testLogger(), false)
	rollupRows, err := j.aggregateTrafficEvents(context.Background(), tx, bucket, bucket.Add(bucketDuration5m))
	if err != nil {
		t.Fatalf("aggregateTrafficEvents: %v", err)
	}

	wantDim := classKey("RATE_LIMITED", "4xx", "openai", "gpt-4o-mini")
	var haveCount, haveSeen bool
	for _, r := range rollupRows {
		if r.DimensionKey != wantDim || r.SubDimension != "source=vk" {
			continue
		}
		switch r.MetricName {
		case metrics.MetricTrafficErrorClassCount:
			haveCount = r.Value == 1
		case metrics.MetricTrafficErrorClassSeen:
			haveSeen = len(r.Metadata) > 0
		}
	}
	if !haveCount {
		t.Errorf("missing/wrong %s row for %s", metrics.MetricTrafficErrorClassCount, wantDim)
	}
	if !haveSeen {
		t.Errorf("missing %s metadata row for %s", metrics.MetricTrafficErrorClassSeen, wantDim)
	}
}

// TestErrorClassAcc_PartTruncationAndHardCap: provider/model parts truncate
// at errorClassPartMaxLen (bounding per-class key size), and once
// errorClassBucketHardCap distinct classes are admitted, NEW classes fold
// straight into the per-status-range overflow at observe time so accumulator
// memory cannot grow with a unique-name spray.
func TestErrorClassAcc_PartTruncationAndHardCap(t *testing.T) {
	acc := newErrorClassAcc()
	ts := time.Now().UTC()

	long := strings.Repeat("x", errorClassPartMaxLen+500)
	acc.observe(intPtr(400), nil, strPtr("invalid_request"), nil, strPtr("p"), strPtr(long), nil, "source=vk", ts)
	wantModel := long[:errorClassPartMaxLen]
	k := accKey5m{metrics.MetricTrafficErrorClassCount, classKey("invalid_request", "4xx", "p", wantModel), "source=vk"}
	if acc.counts[k] != 1 {
		t.Error("truncated-model class not recorded under the truncated key")
	}

	// Fill to the hard cap, then spray two more classes (one per range).
	acc2 := newErrorClassAcc()
	for i := range errorClassBucketHardCap {
		acc2.observe(intPtr(400), nil, strPtr("invalid_request"), strPtr("p"), nil, strPtr(fmt.Sprintf("m-%05d", i)), nil, "source=vk", ts)
	}
	acc2.observe(intPtr(404), nil, strPtr("ROUTING_NO_MATCH"), strPtr("p"), nil, strPtr("brand-new-4xx"), nil, "source=vk", ts)
	acc2.observe(intPtr(502), nil, strPtr("PROVIDER_ERROR"), strPtr("p"), nil, strPtr("brand-new-5xx"), nil, "source=vk", ts)

	if len(acc2.classTotals) != errorClassBucketHardCap+2 {
		// hard cap classes + the two overflow classes (4xx and 5xx)
		t.Fatalf("classTotals = %d, want hardCap+2 overflow entries", len(acc2.classTotals))
	}
	ovf4 := accKey5m{metrics.MetricTrafficErrorClassCount, classKey(metrics.ErrorClassOverflowCode, "4xx", "", ""), "source=vk"}
	ovf5 := accKey5m{metrics.MetricTrafficErrorClassCount, classKey(metrics.ErrorClassOverflowCode, "5xx", "", ""), "source=vk"}
	if acc2.counts[ovf4] != 1 || acc2.counts[ovf5] != 1 {
		t.Errorf("post-cap classes not folded to overflow: 4xx=%v 5xx=%v", acc2.counts[ovf4], acc2.counts[ovf5])
	}
	// An ALREADY-admitted class keeps accumulating under its own key.
	acc2.observe(intPtr(400), nil, strPtr("invalid_request"), strPtr("p"), nil, strPtr("m-00000"), nil, "source=vk", ts)
	known := accKey5m{metrics.MetricTrafficErrorClassCount, classKey("invalid_request", "4xx", "p", "m-00000"), "source=vk"}
	if acc2.counts[known] != 2 {
		t.Errorf("admitted class count = %v, want 2", acc2.counts[known])
	}
}

// TestCoalesceName_UTF8SafeTruncation: truncation must cut on a rune
// boundary — a mid-rune byte slice would produce an invalid-UTF-8 dimension
// key that PostgreSQL rejects, failing the whole bucket insert.
func TestCoalesceName_UTF8SafeTruncation(t *testing.T) {
	// 255 ASCII bytes then a 3-byte rune spanning the 256-byte boundary.
	name := strings.Repeat("a", errorClassPartMaxLen-1) + "中文"
	got := coalesceName(strPtr(name), nil)
	if len(got) > errorClassPartMaxLen {
		t.Errorf("truncated length = %d > max", len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncated name is invalid UTF-8: %q", got[len(got)-4:])
	}
	if got != strings.Repeat("a", errorClassPartMaxLen-1) {
		t.Errorf("expected cut before the spanning rune, got len %d", len(got))
	}
}
