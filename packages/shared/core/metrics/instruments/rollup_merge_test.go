package instruments

import (
	"github.com/goccy/go-json"
	"testing"
	"time"
)

// MergeThingRollupRows — per-Thing twin of MergeRollupRows.
// Asserts: ThingID isolation; insertion order; sum / histogram / timestamp
// branches; empty-input edge.

func TestMergeThingRollupRowsSumAndIsolation(t *testing.T) {
	rows := []ThingRollupRow{
		{ThingID: "agent-1", MetricName: MetricRequestCount, DimensionKey: "provider=openai", Value: 10},
		{ThingID: "agent-1", MetricName: MetricRequestCount, DimensionKey: "provider=openai", Value: 25},
		// Same dimension key but DIFFERENT ThingID — must not merge with agent-1.
		{ThingID: "agent-2", MetricName: MetricRequestCount, DimensionKey: "provider=openai", Value: 7},
		{ThingID: "agent-1", MetricName: MetricRequestCount, DimensionKey: "provider=anthropic", Value: 5},
	}

	merged := MergeThingRollupRows(rows)
	if len(merged) != 3 {
		t.Fatalf("merged rows = %d, want 3 (agent-1/openai, agent-2/openai, agent-1/anthropic)", len(merged))
	}

	// Insertion-order preserved.
	if merged[0].ThingID != "agent-1" || merged[0].DimensionKey != "provider=openai" {
		t.Errorf("merged[0] = (%s, %s), want (agent-1, provider=openai)", merged[0].ThingID, merged[0].DimensionKey)
	}
	if merged[0].Value != 35 {
		t.Errorf("merged[0].Value = %v, want 35 (10+25)", merged[0].Value)
	}
	// agent-2 must remain its own row.
	if merged[1].ThingID != "agent-2" {
		t.Errorf("merged[1].ThingID = %q, want agent-2 (cross-Thing isolation)", merged[1].ThingID)
	}
	if merged[1].Value != 7 {
		t.Errorf("merged[1].Value = %v, want 7 (no leak from agent-1)", merged[1].Value)
	}
	if merged[2].ThingID != "agent-1" || merged[2].DimensionKey != "provider=anthropic" {
		t.Errorf("merged[2] = (%s, %s), want (agent-1, provider=anthropic)", merged[2].ThingID, merged[2].DimensionKey)
	}
}

func TestMergeThingRollupRowsHistogram(t *testing.T) {
	h1 := Histogram{1, 2, 3, 4, 5, 6}
	h2 := Histogram{10, 20, 30, 40, 50, 60}
	want := Histogram{11, 22, 33, 44, 55, 66}

	m1, _ := json.Marshal(h1)
	m2, _ := json.Marshal(h2)

	rows := []ThingRollupRow{
		{ThingID: "tA", MetricName: MetricLatencyHistogram, Metadata: m1},
		{ThingID: "tA", MetricName: MetricLatencyHistogram, Metadata: m2},
	}
	merged := MergeThingRollupRows(rows)
	if len(merged) != 1 {
		t.Fatalf("merged rows = %d, want 1", len(merged))
	}
	got, err := ParseHistogramMetadata(merged[0].Metadata)
	if err != nil {
		t.Fatalf("ParseHistogramMetadata: %v", err)
	}
	if got != want {
		t.Errorf("histogram merge = %v, want %v", got, want)
	}
}

func TestMergeThingRollupRowsTimestamp(t *testing.T) {
	m1, _ := json.Marshal(TimestampMeta{FirstSeen: "2026-04-10T00:00:00Z", LastSeen: "2026-04-12T00:00:00Z"})
	m2, _ := json.Marshal(TimestampMeta{FirstSeen: "2026-04-08T00:00:00Z", LastSeen: "2026-04-15T00:00:00Z"})

	rows := []ThingRollupRow{
		{ThingID: "tA", MetricName: MetricFirstSeen, Metadata: m1},
		{ThingID: "tA", MetricName: MetricFirstSeen, Metadata: m2},
	}
	merged := MergeThingRollupRows(rows)
	if len(merged) != 1 {
		t.Fatalf("merged rows = %d, want 1", len(merged))
	}
	var got TimestampMeta
	if err := json.Unmarshal(merged[0].Metadata, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.FirstSeen != "2026-04-08T00:00:00Z" {
		t.Errorf("FirstSeen = %q, want MIN 2026-04-08T00:00:00Z", got.FirstSeen)
	}
	if got.LastSeen != "2026-04-15T00:00:00Z" {
		t.Errorf("LastSeen = %q, want MAX 2026-04-15T00:00:00Z", got.LastSeen)
	}
}

func TestMergeThingRollupRowsEmpty(t *testing.T) {
	out := MergeThingRollupRows(nil)
	if len(out) != 0 {
		t.Errorf("nil input → len %d, want 0", len(out))
	}
	out = MergeThingRollupRows([]ThingRollupRow{})
	if len(out) != 0 {
		t.Errorf("empty input → len %d, want 0", len(out))
	}
}

// mergeHistogramMetadataThing — error branches.
// Asserts: unparseable dst.Metadata overwrites with src; unparseable src is
// skipped; success path produces element-wise sum.

func TestMergeThingRollupRowsHistogramDstUnparseable(t *testing.T) {
	// dst (first occurrence) carries garbage metadata; src is valid →
	// dst.Metadata must be replaced with src's bytes.
	hValid := Histogram{2, 4, 6, 8, 10, 12}
	srcBytes, _ := json.Marshal(hValid)

	rows := []ThingRollupRow{
		{ThingID: "tA", MetricName: MetricLatencyHistogram, Metadata: json.RawMessage(`not-json`)},
		{ThingID: "tA", MetricName: MetricLatencyHistogram, Metadata: srcBytes},
	}
	merged := MergeThingRollupRows(rows)
	if len(merged) != 1 {
		t.Fatalf("merged rows = %d, want 1", len(merged))
	}
	got, err := ParseHistogramMetadata(merged[0].Metadata)
	if err != nil {
		t.Fatalf("ParseHistogramMetadata after dst-replace: %v", err)
	}
	if got != hValid {
		t.Errorf("histogram = %v, want %v (dst overwritten by src on parse error)", got, hValid)
	}
}

func TestMergeThingRollupRowsHistogramSrcUnparseable(t *testing.T) {
	// dst valid, src garbage → src silently skipped, dst unchanged.
	hValid := Histogram{2, 4, 6, 8, 10, 12}
	dstBytes, _ := json.Marshal(hValid)

	rows := []ThingRollupRow{
		{ThingID: "tA", MetricName: MetricLatencyHistogram, Metadata: dstBytes},
		{ThingID: "tA", MetricName: MetricLatencyHistogram, Metadata: json.RawMessage(`broken`)},
	}
	merged := MergeThingRollupRows(rows)
	if len(merged) != 1 {
		t.Fatalf("merged rows = %d, want 1", len(merged))
	}
	got, err := ParseHistogramMetadata(merged[0].Metadata)
	if err != nil {
		t.Fatalf("ParseHistogramMetadata: %v", err)
	}
	if got != hValid {
		t.Errorf("histogram = %v, want %v (src skipped on parse error)", got, hValid)
	}
}

// mergeTimestampMetadataThing — error branches.

func TestMergeThingRollupRowsTimestampDstUnparseable(t *testing.T) {
	srcBytes, _ := json.Marshal(TimestampMeta{FirstSeen: "2026-04-08T00:00:00Z", LastSeen: "2026-04-15T00:00:00Z"})

	rows := []ThingRollupRow{
		{ThingID: "tA", MetricName: MetricFirstSeen, Metadata: json.RawMessage(`not-json`)},
		{ThingID: "tA", MetricName: MetricFirstSeen, Metadata: srcBytes},
	}
	merged := MergeThingRollupRows(rows)
	if len(merged) != 1 {
		t.Fatalf("merged rows = %d, want 1", len(merged))
	}
	// Per impl: when dst metadata unparseable, dst.Metadata = src.Metadata and return.
	var got TimestampMeta
	if err := json.Unmarshal(merged[0].Metadata, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.FirstSeen != "2026-04-08T00:00:00Z" || got.LastSeen != "2026-04-15T00:00:00Z" {
		t.Errorf("ts meta = %+v, want overwritten by src", got)
	}
}

func TestMergeThingRollupRowsTimestampSrcUnparseable(t *testing.T) {
	dstBytes, _ := json.Marshal(TimestampMeta{FirstSeen: "2026-04-10T00:00:00Z", LastSeen: "2026-04-12T00:00:00Z"})

	rows := []ThingRollupRow{
		{ThingID: "tA", MetricName: MetricFirstSeen, Metadata: dstBytes},
		{ThingID: "tA", MetricName: MetricFirstSeen, Metadata: json.RawMessage(`broken`)},
	}
	merged := MergeThingRollupRows(rows)
	if len(merged) != 1 {
		t.Fatalf("merged rows = %d, want 1", len(merged))
	}
	var got TimestampMeta
	if err := json.Unmarshal(merged[0].Metadata, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Src skipped → dst preserved.
	if got.FirstSeen != "2026-04-10T00:00:00Z" || got.LastSeen != "2026-04-12T00:00:00Z" {
		t.Errorf("ts meta = %+v, want dst preserved", got)
	}
}

// mergeHistogramMetadata (RollupRow, non-Thing) — error branches.
// Mirrors the Thing-twin cases above.

func TestMergeRollupRowsHistogramDstUnparseable(t *testing.T) {
	hValid := Histogram{2, 4, 6, 8, 10, 12}
	srcBytes, _ := json.Marshal(hValid)

	rows := []RollupRow{
		{MetricName: MetricLatencyHistogram, DimensionKey: "p=x", Metadata: json.RawMessage(`bad`)},
		{MetricName: MetricLatencyHistogram, DimensionKey: "p=x", Metadata: srcBytes},
	}
	merged := MergeRollupRows(rows)
	if len(merged) != 1 {
		t.Fatalf("merged = %d, want 1", len(merged))
	}
	got, err := ParseHistogramMetadata(merged[0].Metadata)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != hValid {
		t.Errorf("histogram = %v, want %v (dst replaced by src)", got, hValid)
	}
}

func TestMergeRollupRowsHistogramSrcUnparseable(t *testing.T) {
	hValid := Histogram{2, 4, 6, 8, 10, 12}
	dstBytes, _ := json.Marshal(hValid)

	rows := []RollupRow{
		{MetricName: MetricLatencyHistogram, DimensionKey: "p=x", Metadata: dstBytes},
		{MetricName: MetricLatencyHistogram, DimensionKey: "p=x", Metadata: json.RawMessage(`bad`)},
	}
	merged := MergeRollupRows(rows)
	if len(merged) != 1 {
		t.Fatalf("merged = %d, want 1", len(merged))
	}
	got, err := ParseHistogramMetadata(merged[0].Metadata)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != hValid {
		t.Errorf("histogram = %v, want %v (src skipped, dst unchanged)", got, hValid)
	}
}

// mergeTimestampMetadata (RollupRow, non-Thing) — error branches.

func TestMergeRollupRowsTimestampDstUnparseable(t *testing.T) {
	srcBytes, _ := json.Marshal(TimestampMeta{FirstSeen: "2026-04-08T00:00:00Z", LastSeen: "2026-04-15T00:00:00Z"})

	rows := []RollupRow{
		{MetricName: MetricFirstSeen, DimensionKey: "p=x", Metadata: json.RawMessage(`bad`)},
		{MetricName: MetricFirstSeen, DimensionKey: "p=x", Metadata: srcBytes},
	}
	merged := MergeRollupRows(rows)
	if len(merged) != 1 {
		t.Fatalf("merged = %d, want 1", len(merged))
	}
	var got TimestampMeta
	if err := json.Unmarshal(merged[0].Metadata, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.FirstSeen != "2026-04-08T00:00:00Z" {
		t.Errorf("FirstSeen = %q, want src-overwritten 2026-04-08T00:00:00Z", got.FirstSeen)
	}
}

func TestMergeRollupRowsTimestampSrcUnparseable(t *testing.T) {
	dstBytes, _ := json.Marshal(TimestampMeta{FirstSeen: "2026-04-10T00:00:00Z", LastSeen: "2026-04-12T00:00:00Z"})

	rows := []RollupRow{
		{MetricName: MetricFirstSeen, DimensionKey: "p=x", Metadata: dstBytes},
		{MetricName: MetricFirstSeen, DimensionKey: "p=x", Metadata: json.RawMessage(`bad`)},
	}
	merged := MergeRollupRows(rows)
	if len(merged) != 1 {
		t.Fatalf("merged = %d, want 1", len(merged))
	}
	var got TimestampMeta
	if err := json.Unmarshal(merged[0].Metadata, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.FirstSeen != "2026-04-10T00:00:00Z" {
		t.Errorf("FirstSeen = %q, want dst-preserved 2026-04-10T00:00:00Z", got.FirstSeen)
	}
}

// buildSummaryMetadata — error branches (invalid histogram bytes, timestamp
// arm, invalid timestamp bytes) reached via BuildResult summary path.
// Asserts: invalid metadata rows are silently skipped; valid rows still
// surface in result.Metadata.

func TestBuildResultSummaryMetadataInvalidHistogram(t *testing.T) {
	hValid := Histogram{1, 2, 3, 4, 5, 6}
	hBytes, _ := json.Marshal(hValid)

	rows := []RollupRow{
		{MetricName: MetricLatencyHistogram, Metadata: json.RawMessage(`{"buckets": "garbage"}`)},
		{MetricName: MetricLatencyHistogram, Metadata: hBytes},
	}
	q := MetricsQuery{Metrics: []string{MetricLatencyHistogram}}
	result := BuildResult(q, rows, Granularity1h)

	if result.Metadata == nil {
		t.Fatal("Metadata nil — valid histogram should surface")
	}
	merged, ok := result.Metadata[MetricLatencyHistogram].(Histogram)
	if !ok {
		t.Fatalf("Metadata type = %T, want Histogram", result.Metadata[MetricLatencyHistogram])
	}
	if merged != hValid {
		t.Errorf("merged histogram = %v, want %v (invalid bytes skipped)", merged, hValid)
	}
}

func TestBuildResultSummaryMetadataTimestamp(t *testing.T) {
	ts1, _ := json.Marshal(TimestampMeta{FirstSeen: "2026-04-10T00:00:00Z", LastSeen: "2026-04-11T00:00:00Z"})
	ts2, _ := json.Marshal(TimestampMeta{FirstSeen: "2026-04-08T00:00:00Z", LastSeen: "2026-04-15T00:00:00Z"})

	rows := []RollupRow{
		{MetricName: MetricFirstSeen, Metadata: ts1},
		{MetricName: MetricFirstSeen, Metadata: ts2},
		// Empty metadata should be ignored (the len==0 short-circuit).
		{MetricName: MetricFirstSeen, Metadata: nil},
	}
	q := MetricsQuery{Metrics: []string{MetricFirstSeen}}
	result := BuildResult(q, rows, Granularity1h)
	if result.Metadata == nil {
		t.Fatal("Metadata nil — timestamp meta should surface")
	}
	got, ok := result.Metadata[MetricFirstSeen].(TimestampMeta)
	if !ok {
		t.Fatalf("type = %T, want TimestampMeta", result.Metadata[MetricFirstSeen])
	}
	if got.FirstSeen != "2026-04-08T00:00:00Z" {
		t.Errorf("FirstSeen = %q, want MIN 2026-04-08T00:00:00Z", got.FirstSeen)
	}
	if got.LastSeen != "2026-04-15T00:00:00Z" {
		t.Errorf("LastSeen = %q, want MAX 2026-04-15T00:00:00Z", got.LastSeen)
	}
}

func TestBuildResultSummaryMetadataInvalidTimestamp(t *testing.T) {
	tsValid, _ := json.Marshal(TimestampMeta{FirstSeen: "2026-04-10T00:00:00Z", LastSeen: "2026-04-12T00:00:00Z"})

	rows := []RollupRow{
		{MetricName: MetricFirstSeen, Metadata: json.RawMessage(`{"first_seen": 42}`)}, // type-mismatch
		{MetricName: MetricFirstSeen, Metadata: tsValid},
	}
	q := MetricsQuery{Metrics: []string{MetricFirstSeen}}
	result := BuildResult(q, rows, Granularity1h)
	if result.Metadata == nil {
		t.Fatal("Metadata nil — valid ts should still surface")
	}
	got, ok := result.Metadata[MetricFirstSeen].(TimestampMeta)
	if !ok {
		t.Fatalf("type = %T, want TimestampMeta", result.Metadata[MetricFirstSeen])
	}
	if got.FirstSeen != "2026-04-10T00:00:00Z" {
		t.Errorf("FirstSeen = %q, want 2026-04-10 (invalid row skipped)", got.FirstSeen)
	}
}

func TestBuildResultSummaryMetadataAllEmpty(t *testing.T) {
	// No rows with metadata, no histograms, no timestamps → Metadata must be nil.
	rows := []RollupRow{
		{MetricName: MetricRequestCount, Value: 1},
	}
	q := MetricsQuery{Metrics: []string{MetricRequestCount}}
	result := BuildResult(q, rows, Granularity1h)
	if result.Metadata != nil {
		t.Errorf("Metadata = %v, want nil when no histogram/timestamp rows", result.Metadata)
	}
}

// buildGroupMetadata — histogram + timestamp branches via grouped query path.

func TestBuildResultGroupMetadataHistogram(t *testing.T) {
	h1 := Histogram{1, 0, 0, 0, 0, 0}
	h2 := Histogram{0, 2, 0, 0, 0, 0}
	h3 := Histogram{0, 0, 3, 0, 0, 0}
	b1, _ := json.Marshal(h1)
	b2, _ := json.Marshal(h2)
	b3, _ := json.Marshal(h3)

	rows := []RollupRow{
		{MetricName: MetricLatencyHistogram, DimensionKey: "provider=openai", Value: 0, Metadata: b1},
		{MetricName: MetricLatencyHistogram, DimensionKey: "provider=openai", Value: 0, Metadata: b2},
		{MetricName: MetricLatencyHistogram, DimensionKey: "provider=anthropic", Value: 0, Metadata: b3},
	}
	q := MetricsQuery{Metrics: []string{MetricLatencyHistogram}, DimensionKey: "provider"}
	result := BuildResult(q, rows, Granularity1h)

	if result.Metadata == nil {
		t.Fatal("Metadata nil")
	}
	// Key format is "<dimensionKey>:<metricName>".
	keyOpenAI := "provider=openai:" + MetricLatencyHistogram
	mergedOpenAI, ok := result.Metadata[keyOpenAI].(Histogram)
	if !ok {
		t.Fatalf("Metadata[%q] type = %T, want Histogram", keyOpenAI, result.Metadata[keyOpenAI])
	}
	wantOpenAI := Histogram{1, 2, 0, 0, 0, 0}
	if mergedOpenAI != wantOpenAI {
		t.Errorf("openai histogram = %v, want %v", mergedOpenAI, wantOpenAI)
	}
	keyAnthropic := "provider=anthropic:" + MetricLatencyHistogram
	mergedAnthropic, ok := result.Metadata[keyAnthropic].(Histogram)
	if !ok {
		t.Fatalf("Metadata[%q] type = %T", keyAnthropic, result.Metadata[keyAnthropic])
	}
	if mergedAnthropic != h3 {
		t.Errorf("anthropic histogram = %v, want %v", mergedAnthropic, h3)
	}
}

func TestBuildResultGroupMetadataHistogramInvalidSkipped(t *testing.T) {
	hValid := Histogram{1, 2, 3, 4, 5, 6}
	hBytes, _ := json.Marshal(hValid)

	rows := []RollupRow{
		{MetricName: MetricLatencyHistogram, DimensionKey: "provider=openai", Metadata: json.RawMessage(`{"buckets": "x"}`)},
		{MetricName: MetricLatencyHistogram, DimensionKey: "provider=openai", Metadata: hBytes},
	}
	q := MetricsQuery{Metrics: []string{MetricLatencyHistogram}, DimensionKey: "provider"}
	result := BuildResult(q, rows, Granularity1h)
	if result.Metadata == nil {
		t.Fatal("Metadata nil")
	}
	key := "provider=openai:" + MetricLatencyHistogram
	got, ok := result.Metadata[key].(Histogram)
	if !ok {
		t.Fatalf("type = %T", result.Metadata[key])
	}
	if got != hValid {
		t.Errorf("got %v, want %v (invalid row skipped)", got, hValid)
	}
}

func TestBuildResultGroupMetadataTimestamp(t *testing.T) {
	ts1, _ := json.Marshal(TimestampMeta{FirstSeen: "2026-04-10T00:00:00Z", LastSeen: "2026-04-11T00:00:00Z"})
	ts2, _ := json.Marshal(TimestampMeta{FirstSeen: "2026-04-08T00:00:00Z", LastSeen: "2026-04-15T00:00:00Z"})
	ts3, _ := json.Marshal(TimestampMeta{FirstSeen: "2026-04-09T00:00:00Z", LastSeen: "2026-04-13T00:00:00Z"})

	rows := []RollupRow{
		{MetricName: MetricFirstSeen, DimensionKey: "provider=openai", Metadata: ts1},
		{MetricName: MetricFirstSeen, DimensionKey: "provider=openai", Metadata: ts2},
		{MetricName: MetricFirstSeen, DimensionKey: "provider=anthropic", Metadata: ts3},
		// Empty metadata row in a different group — exercises the skip branch.
		{MetricName: MetricFirstSeen, DimensionKey: "provider=anthropic", Metadata: nil},
	}
	q := MetricsQuery{Metrics: []string{MetricFirstSeen}, DimensionKey: "provider"}
	result := BuildResult(q, rows, Granularity1h)
	if result.Metadata == nil {
		t.Fatal("Metadata nil")
	}
	keyOpenAI := "provider=openai:" + MetricFirstSeen
	gotOpenAI, ok := result.Metadata[keyOpenAI].(TimestampMeta)
	if !ok {
		t.Fatalf("type = %T", result.Metadata[keyOpenAI])
	}
	if gotOpenAI.FirstSeen != "2026-04-08T00:00:00Z" {
		t.Errorf("openai FirstSeen = %q, want MIN 2026-04-08", gotOpenAI.FirstSeen)
	}
	if gotOpenAI.LastSeen != "2026-04-15T00:00:00Z" {
		t.Errorf("openai LastSeen = %q, want MAX 2026-04-15", gotOpenAI.LastSeen)
	}
	keyAnthropic := "provider=anthropic:" + MetricFirstSeen
	gotAnthropic, ok := result.Metadata[keyAnthropic].(TimestampMeta)
	if !ok {
		t.Fatalf("type = %T", result.Metadata[keyAnthropic])
	}
	if gotAnthropic.FirstSeen != "2026-04-09T00:00:00Z" {
		t.Errorf("anthropic FirstSeen = %q, want 2026-04-09", gotAnthropic.FirstSeen)
	}
}

func TestBuildResultGroupMetadataTimestampInvalidSkipped(t *testing.T) {
	tsValid, _ := json.Marshal(TimestampMeta{FirstSeen: "2026-04-10T00:00:00Z", LastSeen: "2026-04-12T00:00:00Z"})

	rows := []RollupRow{
		{MetricName: MetricFirstSeen, DimensionKey: "provider=openai", Metadata: json.RawMessage(`{"first_seen": 12345}`)},
		{MetricName: MetricFirstSeen, DimensionKey: "provider=openai", Metadata: tsValid},
	}
	q := MetricsQuery{Metrics: []string{MetricFirstSeen}, DimensionKey: "provider"}
	result := BuildResult(q, rows, Granularity1h)
	if result.Metadata == nil {
		t.Fatal("Metadata nil")
	}
	key := "provider=openai:" + MetricFirstSeen
	got, ok := result.Metadata[key].(TimestampMeta)
	if !ok {
		t.Fatalf("type = %T", result.Metadata[key])
	}
	if got.FirstSeen != "2026-04-10T00:00:00Z" {
		t.Errorf("FirstSeen = %q, want 2026-04-10 (invalid row skipped)", got.FirstSeen)
	}
}

func TestBuildResultGroupMetadataAllEmpty(t *testing.T) {
	// Grouped query with no histogram/timestamp rows → Metadata nil.
	rows := []RollupRow{
		{MetricName: MetricRequestCount, DimensionKey: "provider=openai", Value: 5},
	}
	q := MetricsQuery{Metrics: []string{MetricRequestCount}, DimensionKey: "provider"}
	result := BuildResult(q, rows, Granularity1h)
	if result.Metadata != nil {
		t.Errorf("Metadata = %v, want nil", result.Metadata)
	}
}

// buildSummary / buildGroups / buildTimeSeries — filtered-out metric branch.
// Asserts: rows whose MetricName is not in the requested set are excluded.

func TestBuildResultSummaryFiltersOutUnrequestedMetric(t *testing.T) {
	rows := []RollupRow{
		{MetricName: MetricRequestCount, Value: 10},
		{MetricName: MetricPromptTokens, Value: 999}, // not requested
	}
	q := MetricsQuery{Metrics: []string{MetricRequestCount}}
	result := BuildResult(q, rows, Granularity1h)
	if v, ok := result.Summary[MetricPromptTokens]; ok {
		t.Errorf("Summary[%q] = %v present, want absent", MetricPromptTokens, v)
	}
	if result.Summary[MetricRequestCount] != 10 {
		t.Errorf("Summary[%q] = %v, want 10", MetricRequestCount, result.Summary[MetricRequestCount])
	}
}

func TestBuildResultGroupsFiltersOutUnrequestedMetric(t *testing.T) {
	rows := []RollupRow{
		{MetricName: MetricRequestCount, DimensionKey: "provider=openai", Value: 10},
		{MetricName: MetricPromptTokens, DimensionKey: "provider=openai", Value: 999}, // filtered out
	}
	q := MetricsQuery{Metrics: []string{MetricRequestCount}, DimensionKey: "provider"}
	result := BuildResult(q, rows, Granularity1h)
	if len(result.Groups) != 1 {
		t.Fatalf("Groups = %d, want 1", len(result.Groups))
	}
	if _, ok := result.Groups[0].Values[MetricPromptTokens]; ok {
		t.Errorf("Groups[0] should not contain filtered metric %q", MetricPromptTokens)
	}
}

func TestBuildResultTimeSeriesFiltersOutUnrequestedMetric(t *testing.T) {
	t1 := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	rows := []RollupRow{
		{MetricName: MetricRequestCount, BucketStart: t1, Value: 10},
		{MetricName: MetricPromptTokens, BucketStart: t1, Value: 999}, // filtered out
	}
	q := MetricsQuery{Metrics: []string{MetricRequestCount}, TimeSeries: true}
	result := BuildResult(q, rows, Granularity1h)
	if len(result.Series) != 1 {
		t.Fatalf("Series = %d, want 1", len(result.Series))
	}
	if _, ok := result.Series[0].Values[MetricPromptTokens]; ok {
		t.Errorf("Series[0] should not contain filtered metric %q", MetricPromptTokens)
	}
}

// Granularity defaults — BucketDuration / TruncateTime fallthrough.
// Asserts: unknown granularity falls back to hourly behavior.

func TestGranularityBucketDurationDefault(t *testing.T) {
	unknown := Granularity("unknown-gran")
	got := unknown.BucketDuration()
	if got != time.Hour {
		t.Errorf("Granularity(%q).BucketDuration() = %v, want fallback time.Hour", unknown, got)
	}
}

func TestGranularityTruncateTimeDefault(t *testing.T) {
	unknown := Granularity("unknown-gran")
	ts := time.Date(2026, 4, 15, 14, 37, 42, 123456789, time.UTC)
	got := unknown.TruncateTime(ts)
	want := ts.Truncate(time.Hour)
	if !got.Equal(want) {
		t.Errorf("Granularity(%q).TruncateTime(%v) = %v, want fallback truncate-to-hour %v", unknown, ts, got, want)
	}
}

// Percentile — last-bucket clamping (hi>1e8) and the zero-count `continue`
// branch and the structural fallback (target never reached).

func TestPercentileLastBucketClamp(t *testing.T) {
	// All 100 samples land in the LAST bucket (index 5, upper bound = 1e9).
	// This forces the `hi > 1e8` branch — Percentile should clamp hi to
	// 2*lowerBound = 2000ms instead of using the absurd 1e9 boundary.
	h := Histogram{0, 0, 0, 0, 0, 100}
	p := h.Percentile(0.5)
	// lo = 1000, hi clamped to 2000. fraction at p=0.5 over 100 samples = 0.5
	// → result ≈ 1000 + 0.5*(2000-1000) = 1500.
	if p < 1000 || p > 2000 {
		t.Errorf("Percentile(0.5) in last bucket = %v, want clamped into [1000, 2000]", p)
	}
}

func TestPercentileLastBucketClampZeroLower(t *testing.T) {
	// Edge: if lowerBound is somehow zero AND we hit the hi>1e8 path, the
	// impl sets hi=1 to avoid the lo=hi=0 collapse. We cannot trigger this
	// directly with current boundaries (lowerBound(5)=1000), so this test
	// documents the structural invariant by exercising the only-last-bucket
	// flavor and verifying it returns a finite, non-negative value.
	h := Histogram{0, 0, 0, 0, 0, 1}
	p := h.Percentile(1.0)
	if p < 0 {
		t.Errorf("Percentile(1.0) = %v, want non-negative", p)
	}
}

func TestPercentileSkipsZeroCountBucket(t *testing.T) {
	// Buckets [0]=0 (skipped via `continue`), [1]=10 sample.
	// p=0.5 → target=5. Cumulative reaches 10 in bucket 1, returns
	// lowerBound(1)=50 + 0.5*(100-50) = 75.
	h := Histogram{0, 10, 0, 0, 0, 0}
	p := h.Percentile(0.5)
	if p < 50 || p > 100 {
		t.Errorf("Percentile(0.5) = %v, want in [50, 100)", p)
	}
}

func TestPercentileTargetZero(t *testing.T) {
	// p=0 → target=0. The first bucket with count>0 will satisfy
	// `cumulative >= 0` immediately (prev=0, fraction=0/c=0), returning
	// the lower bound of that bucket.
	h := Histogram{0, 10, 0, 0, 0, 0}
	p := h.Percentile(0)
	if p != 50 { // lowerBound(1) = 50
		t.Errorf("Percentile(0) = %v, want 50 (lowerBound of first populated bucket)", p)
	}
}

// BucketForLatency — explicit boundary trip just past the last boundary
// (already covered by sibling test, retained here to pin the +∞ fallthrough).

func TestBucketForLatencyAboveLastBoundary(t *testing.T) {
	// Anything >= 1e9 falls through the loop and returns the final bucket
	// index (HistogramBucketCount-1 == 5).
	got := BucketForLatency(1e9)
	if got != HistogramBucketCount-1 {
		t.Errorf("BucketForLatency(1e9) = %d, want %d", got, HistogramBucketCount-1)
	}
	got = BucketForLatency(1e12)
	if got != HistogramBucketCount-1 {
		t.Errorf("BucketForLatency(1e12) = %d, want %d", got, HistogramBucketCount-1)
	}
}
