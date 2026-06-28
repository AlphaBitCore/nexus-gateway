package consumer

import (
	"reflect"
	"testing"
	"time"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// poolParityEvents returns a spread of events covering pointer/value/JSON/array
// columns, an absent-field row, and bodies, so the parity comparison exercises
// every boxing path the COPY row builder takes.
func poolParityEvents() []TrafficEventMessage {
	s := func(v string) *string { return &v }
	i := func(v int) *int { return &v }
	f := func(v float64) *float64 { return &v }
	ts := time.Unix(1_700_000_000, 0).UTC()
	return []TrafficEventMessage{
		{
			ID: "e1", Source: "ai-gateway", Timestamp: ts,
			TraceID: s("t1"), StatusCode: i(200), LatencyMs: i(42),
			EntityType: s("user"), EntityID: s("u1"), ProviderID: s("openai"), ModelID: s("gpt-4o"),
			PromptTokens: i(24), CompletionTokens: i(78), TotalTokens: i(102), EstimatedCostUSD: f(0.0012),
			ComplianceTags: []string{"pii", "secret"},
			Identity:       []byte(`{"status":"matched"}`),
			Details:        []byte(`{"requestId":"r1"}`),
			SourceProcess:  s("chat"), Action: s("traffic"), CredentialID: s("c1"),
			EndpointType: "chat", IngressFormat: "openai",
		},
		// All-absent / zero row (every pointer nil, empty JSON) — the nil-promotion
		// and empty-array-default paths.
		{ID: "e2", Source: "ai-gateway", Timestamp: ts},
		// Inline bodies present (exercises payloadRowValues demux too).
		{
			ID: "e3", Source: "ai-gateway", Timestamp: ts, StatusCode: i(500), ErrorCode: s("UPSTREAM_5XX"),
			RequestBody:  sharedaudit.NewInlineBody([]byte(`{"q":"hi"}`), 10, false, "application/json"),
			ResponseBody: sharedaudit.NewInlineBody([]byte("event: x\n"), 9, true, "text/event-stream"),
		},
		// Spill body (json.RawMessage spill-ref branch) + non-nil passthrough_flags
		// (text[]) + latency_breakdown (jsonb) — the column shapes the first three
		// rows don't reach.
		{
			ID: "e4", Source: "ai-gateway", Timestamp: ts,
			PassthroughFlags: []string{"emergency", "kill-switch"},
			LatencyBreakdown: []byte(`{"upstreamMs":12,"hooksMs":3}`),
			RequestBody: sharedaudit.NewSpillBody(
				&sharedaudit.SpillRef{Backend: "s3", Key: "k/abc", Size: 4096, SHA256: "deadbeef"}, 4096, true, "application/json"),
		},
	}
}

// TestCopyPool_RowParity is the load-bearing correctness guard for the pooled COPY
// backing: the values pgx COPYs MUST be byte-for-byte what the original per-row
// []any build produced, for every column of every row. It also proves the
// pre-sized flat backing keeps each row's sub-slice independent — if filling row N
// reallocated the backing, an earlier row's view would be corrupted and the
// comparison would fail.
func TestCopyPool_RowParity(t *testing.T) {
	events := poolParityEvents()

	// Reference: the original per-row allocation path.
	want := make([][]any, len(events))
	for i, e := range events {
		want[i] = trafficEventRowValues(e)
	}

	// Pooled: fill one pre-sized flat backing, sub-slice per row.
	cols := len(trafficEventColumns)
	buf := trafficCopyBufPool.Get().(*trafficCopyBuf)
	defer buf.release()
	buf.reset(len(events), cols)
	got := make([][]any, 0, len(events))
	for _, e := range events {
		start := len(buf.flat)
		buf.flat = appendTrafficEventRow(buf.flat, e)
		got = append(got, buf.flat[start:len(buf.flat):len(buf.flat)])
	}

	if len(got) != len(want) {
		t.Fatalf("pooled produced %d rows, want %d", len(got), len(want))
	}
	for r := range want {
		if len(got[r]) != len(want[r]) {
			t.Fatalf("row %d: pooled has %d cols, want %d", r, len(got[r]), len(want[r]))
		}
		for c := range want[r] {
			if !reflect.DeepEqual(got[r][c], want[r][c]) {
				t.Errorf("row %d col %d (%s): pooled=%#v want=%#v",
					r, c, trafficEventColumns[c], got[r][c], want[r][c])
			}
		}
	}
}

// TestCopyPool_PayloadRowParity does the same for the traffic_event_payload COPY,
// including that an absent-body event is skipped (ok=false leaves the backing
// unadvanced) exactly as the per-row path returns ok=false.
func TestCopyPool_PayloadRowParity(t *testing.T) {
	events := poolParityEvents()

	type ref struct {
		vals []any
		ok   bool
	}
	want := make([]ref, len(events))
	for i, e := range events {
		v, ok := payloadRowValues(e)
		want[i] = ref{v, ok}
	}

	cols := len(payloadColumns)
	buf := trafficCopyBufPool.Get().(*trafficCopyBuf)
	defer buf.release()
	buf.reset(len(events), cols)
	for i, e := range events {
		start := len(buf.flat)
		next, ok := appendPayloadRow(buf.flat, e)
		if ok != want[i].ok {
			t.Fatalf("event %d: pooled ok=%v, want %v", i, ok, want[i].ok)
		}
		if !ok {
			continue
		}
		buf.flat = next
		row := buf.flat[start:len(buf.flat):len(buf.flat)]
		if len(row) != len(want[i].vals) {
			t.Fatalf("event %d: pooled payload has %d cols, want %d", i, len(row), len(want[i].vals))
		}
		for c := range want[i].vals {
			if !reflect.DeepEqual(row[c], want[i].vals[c]) {
				t.Errorf("event %d col %d (%s): pooled=%#v want=%#v",
					i, c, payloadColumns[c], row[c], want[i].vals[c])
			}
		}
	}
}
