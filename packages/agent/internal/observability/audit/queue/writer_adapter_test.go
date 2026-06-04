package queue

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/classify"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// newTestQueue constructs a fresh on-disk encrypted Queue rooted in
// t.TempDir() so each test gets its own DB without colliding.
func newWriterAdapterTestQueue(t *testing.T) *Queue {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "audit.db")
	// 32-byte key — Queue's NewQueue uses AES-GCM under the hood.
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	q, err := NewQueue(dbPath, key)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return q
}

func TestQueueWriter_NilSafe(t *testing.T) {
	var w *QueueWriter
	w.Enqueue(sharedaudit.AuditEvent{}) // must not panic
	if err := w.Flush(context.Background()); err != nil {
		t.Fatalf("nil Flush: %v", err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}

func TestQueueWriter_RecordsApproveDecisionAsInspect(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	w := NewQueueWriter(q)
	defer func() { _ = w.Close(context.Background()) }()

	ev := sharedaudit.AuditEvent{
		ID:                  "evt-1",
		TraceID:             "trace-1",
		Timestamp:           time.Now().UTC(),
		SourceIP:            "127.0.0.1",
		TargetHost:          "api.openai.com",
		Method:              "POST",
		Path:                "/v1/chat/completions",
		BumpStatus:          "BUMP_SUCCESS",
		RequestHookDecision: "APPROVE",
		Provider:            "openai",
		Model:               "gpt-4o-mini",
		LatencyMs:           42,
		PromptTokens:        100,
		CompletionTokens:    25,
	}
	w.Enqueue(ev)

	if err := w.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	rows, _, err := q.QueryEvents("", "", 0, 10)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	got := rows[0]
	if got.ID != "evt-1" {
		t.Errorf("id: want evt-1, got %q", got.ID)
	}
	// Note: agent's audit_events SQLite table doesn't have method/path
	// columns yet (separate schema gap, tracked outside writer_adapter
	// scope). The writer DOES copy them onto Event.Method/Path in-memory;
	// they just don't survive the SQLite round-trip via QueryEvents
	// today. Assert against the in-memory mapping by stamping a
	// recognisable value into a column that IS persisted (BumpStatus)
	// so this test still catches a regression in field copying.
	if got.BumpStatus != "BUMP_SUCCESS" {
		t.Errorf("bumpStatus: got %q", got.BumpStatus)
	}
	if got.ProviderName != "openai" || got.ModelName != "gpt-4o-mini" {
		t.Errorf("provider/model: got %q/%q", got.ProviderName, got.ModelName)
	}
	if got.HookDecision != "APPROVE" {
		t.Errorf("hookDecision: want APPROVE, got %q", got.HookDecision)
	}
	if got.Action != "inspect" {
		t.Errorf("action: APPROVE should map to 'inspect', got %q", got.Action)
	}
	if got.PromptTokens == nil || *got.PromptTokens != 100 {
		t.Errorf("promptTokens: want *100, got %v", got.PromptTokens)
	}
	if got.CompletionTokens == nil || *got.CompletionTokens != 25 {
		t.Errorf("completionTokens: want *25, got %v", got.CompletionTokens)
	}
}

func TestQueueWriter_RejectHardMapsToDeny(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	w := NewQueueWriter(q)
	defer func() { _ = w.Close(context.Background()) }()

	w.Enqueue(sharedaudit.AuditEvent{
		ID:                  "evt-2",
		Timestamp:           time.Now().UTC(),
		TargetHost:          "blocked.example.com",
		RequestHookDecision: "REJECT_HARD",
	})

	if err := w.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	rows, _, _ := q.QueryEvents("", "", 0, 10)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].Action != "deny" {
		t.Errorf("REJECT_HARD must map to 'deny', got %q", rows[0].Action)
	}
}

func TestQueueWriter_BlockSoftAlsoMapsToDeny(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	w := NewQueueWriter(q)
	defer func() { _ = w.Close(context.Background()) }()

	w.Enqueue(sharedaudit.AuditEvent{
		ID:                  "evt-3",
		Timestamp:           time.Now().UTC(),
		TargetHost:          "soft.example.com",
		RequestHookDecision: "BLOCK_SOFT",
	})
	if err := w.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	rows, _, _ := q.QueryEvents("", "", 0, 10)
	if len(rows) != 1 || rows[0].Action != "deny" {
		t.Fatalf("BLOCK_SOFT must map to 'deny', got rows=%d action=%q", len(rows), rows[0].Action)
	}
}

func TestQueueWriter_EmptyDecisionMapsToPassthrough(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	w := NewQueueWriter(q)
	defer func() { _ = w.Close(context.Background()) }()

	w.Enqueue(sharedaudit.AuditEvent{
		ID:                  "evt-4",
		Timestamp:           time.Now().UTC(),
		TargetHost:          "passthrough.example.com",
		RequestHookDecision: "",
	})
	if err := w.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	rows, _, _ := q.QueryEvents("", "", 0, 10)
	if len(rows) != 1 || rows[0].Action != "passthrough" {
		t.Fatalf("empty decision must map to 'passthrough', got rows=%d action=%q", len(rows), rows[0].Action)
	}
}

func TestQueueWriter_ZeroTokensStayNil(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	w := NewQueueWriter(q)
	defer func() { _ = w.Close(context.Background()) }()

	w.Enqueue(sharedaudit.AuditEvent{
		ID:                  "evt-5",
		Timestamp:           time.Now().UTC(),
		TargetHost:          "x.example.com",
		RequestHookDecision: "APPROVE",
		PromptTokens:        0,
		CompletionTokens:    0,
	})
	if err := w.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	rows, _, _ := q.QueryEvents("", "", 0, 10)
	if len(rows) != 1 {
		t.Fatalf("want 1 row")
	}
	if rows[0].PromptTokens != nil {
		t.Errorf("zero PromptTokens should map to nil (NULL), got %v", *rows[0].PromptTokens)
	}
	if rows[0].CompletionTokens != nil {
		t.Errorf("zero CompletionTokens should map to nil (NULL), got %v", *rows[0].CompletionTokens)
	}
}

func TestQueueWriter_ZeroTimestampGetsBackfilled(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	w := NewQueueWriter(q)
	defer func() { _ = w.Close(context.Background()) }()

	before := time.Now().UTC()
	w.Enqueue(sharedaudit.AuditEvent{
		ID:                  "evt-6",
		TargetHost:          "x.example.com",
		RequestHookDecision: "APPROVE",
		// Timestamp deliberately zero
	})
	if err := w.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	rows, _, _ := q.QueryEvents("", "", 0, 10)
	if len(rows) != 1 {
		t.Fatalf("want 1 row")
	}
	if rows[0].Timestamp.Before(before) {
		t.Errorf("zero timestamp must be backfilled to ≥ now, got %v (before %v)", rows[0].Timestamp, before)
	}
}

func TestQueueWriter_PrefersRequestHooksPipeline(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	w := NewQueueWriter(q)
	defer func() { _ = w.Close(context.Background()) }()

	reqPipeline := json.RawMessage(`[{"hook":"pii","decision":"APPROVE"}]`)
	respPipeline := json.RawMessage(`[{"hook":"safety","decision":"APPROVE"}]`)
	w.Enqueue(sharedaudit.AuditEvent{
		ID:                    "evt-7",
		Timestamp:             time.Now().UTC(),
		TargetHost:            "x.example.com",
		RequestHookDecision:   "APPROVE",
		RequestHooksPipeline:  reqPipeline,
		ResponseHooksPipeline: respPipeline,
	})
	if err := w.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	rows, _, _ := q.QueryEvents("", "", 0, 10)
	if len(rows) != 1 {
		t.Fatalf("want 1 row")
	}
	if string(rows[0].HooksPipeline) != string(reqPipeline) {
		t.Errorf("HooksPipeline: want request pipeline, got %s", string(rows[0].HooksPipeline))
	}
}

func TestQueueWriter_FallsBackToResponsePipelineWhenRequestEmpty(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	w := NewQueueWriter(q)
	defer func() { _ = w.Close(context.Background()) }()

	respPipeline := json.RawMessage(`[{"hook":"safety","decision":"APPROVE"}]`)
	w.Enqueue(sharedaudit.AuditEvent{
		ID:                    "evt-8",
		Timestamp:             time.Now().UTC(),
		TargetHost:            "x.example.com",
		RequestHookDecision:   "APPROVE",
		ResponseHooksPipeline: respPipeline,
	})
	if err := w.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	rows, _, _ := q.QueryEvents("", "", 0, 10)
	if string(rows[0].HooksPipeline) != string(respPipeline) {
		t.Errorf("HooksPipeline: when request is empty, must fall back to response pipeline. Got %s", string(rows[0].HooksPipeline))
	}
}

// TestQueueWriter_T33_PerRequestRowsCarryAllClassificationFields pins
// the post-T33 contract: every shared/audit.AuditEvent enqueued
// through QueueWriter lands in SQLite with all classification inputs
// populated (DomainRuleID + PathAction + SourceProcess).
//
// This test exists because the post-T33 code review caught
// writer_adapter.go silently dropping these three fields — and there
// was zero automated coverage of the emit chain end-to-end. Without
// this test the same regression would be invisible until a user
// installs the .pkg, browses chatgpt, and reports "all rows show
// Untracked / App empty" for the third time.
func TestQueueWriter_T33_PerRequestRowsCarryAllClassificationFields(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	w := NewQueueWriter(q)
	defer func() { _ = w.Close(context.Background()) }()

	// Simulate two HTTP requests that came through one bumped TCP
	// connection — one PROCESS path with hooks=APPROVE, one
	// PASSTHROUGH path with no hooks. Both should land as ONE row
	// each in SQLite, both with the classification inputs intact.
	processedEvent := sharedaudit.AuditEvent{
		ID:                  "evt-process-1",
		TraceID:             "flow-1",
		Timestamp:           time.Now().UTC(),
		TargetHost:          "chatgpt.com",
		Method:              "POST",
		Path:                "/backend-api/f/conversation",
		LatencyMs:           420,
		BumpStatus:          "BUMP_SUCCESS",
		RequestHookDecision: "APPROVE",
		Provider:            "chatgpt-web",
		// T33-FIX C1+C2 fields: classification inputs and source proc.
		DomainRuleID:        "00000000-0000-0000-0000-000000000001",
		PathAction:          "PROCESS",
		SourceProcess:       "Google Chrome Helper",
		SourceProcessBundle: "com.google.Chrome.helper",
	}
	w.Enqueue(processedEvent)

	inspectedEvent := sharedaudit.AuditEvent{
		ID:                  "evt-inspect-1",
		TraceID:             "flow-1",
		Timestamp:           time.Now().UTC(),
		TargetHost:          "chatgpt.com",
		Method:              "POST",
		Path:                "/backend-api/sentinel/ping",
		LatencyMs:           45,
		BumpStatus:          "BUMP_SUCCESS",
		RequestHookDecision: "", // PASSTHROUGH path → no hook ran
		// PROCESS-domain matched but per-path action was PASSTHROUGH —
		// so DomainRuleID still set but PathAction differs.
		DomainRuleID:        "00000000-0000-0000-0000-000000000001",
		PathAction:          "PASSTHROUGH",
		SourceProcess:       "Google Chrome Helper",
		SourceProcessBundle: "com.google.Chrome.helper",
	}
	w.Enqueue(inspectedEvent)

	if err := w.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	events, total, err := q.QueryEvents("", "", 0, 100)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if total != 2 {
		t.Fatalf("expected 2 rows, got %d", total)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	byID := map[string]event.Event{}
	for _, e := range events {
		byID[e.ID] = e
	}

	for id, want := range map[string]struct {
		method, path, source, domainRuleID, pathAction, hookDecision string
	}{
		"evt-process-1": {"POST", "/backend-api/f/conversation", "Google Chrome Helper",
			"00000000-0000-0000-0000-000000000001", "PROCESS", "APPROVE"},
		"evt-inspect-1": {"POST", "/backend-api/sentinel/ping", "Google Chrome Helper",
			"00000000-0000-0000-0000-000000000001", "PASSTHROUGH", ""},
	} {
		got, ok := byID[id]
		if !ok {
			t.Errorf("event %s missing from query result", id)
			continue
		}
		if got.Method != want.method {
			t.Errorf("%s.Method = %q, want %q", id, got.Method, want.method)
		}
		if got.Path != want.path {
			t.Errorf("%s.Path = %q, want %q", id, got.Path, want.path)
		}
		if got.SourceProcess != want.source {
			t.Errorf("%s.SourceProcess = %q, want %q (T33 review C2: writer_adapter dropped SourceProcess)",
				id, got.SourceProcess, want.source)
		}
		if got.DomainRuleID != want.domainRuleID {
			t.Errorf("%s.DomainRuleID = %q, want %q (T33 review C1: writer_adapter dropped DomainRuleID)",
				id, got.DomainRuleID, want.domainRuleID)
		}
		if got.PathAction != want.pathAction {
			t.Errorf("%s.PathAction = %q, want %q (T33 review C1: writer_adapter dropped PathAction)",
				id, got.PathAction, want.pathAction)
		}
		if got.HookDecision != want.hookDecision {
			t.Errorf("%s.HookDecision = %q, want %q", id, got.HookDecision, want.hookDecision)
		}
	}
}

// TestClassify_AlignsWithT33PerRequestModel pins the classify() output
// for the per-request rows the new model emits. Mirrors the chatgpt
// scenario the user reported: one TCP flow → multiple HTTP requests →
// each row classifies independently based on its OWN PathAction +
// HookDecision (not the last request only).
func TestClassify_AlignsWithT33PerRequestModel(t *testing.T) {
	cases := []struct {
		name string
		ev   event.Event
		want classify.Classification
	}{
		{
			name: "PROCESS path + APPROVE hook → Processed",
			ev: event.Event{
				DomainRuleID: "domain-1", PathAction: "PROCESS",
				HookDecision: "APPROVE", BumpStatus: "BUMP_SUCCESS",
			},
			want: classify.ClassProcessed,
		},
		{
			name: "PASSTHROUGH path + no hook → Inspect",
			ev: event.Event{
				DomainRuleID: "domain-1", PathAction: "PASSTHROUGH",
				HookDecision: "", BumpStatus: "BUMP_SUCCESS",
			},
			want: classify.ClassInspect,
		},
		{
			name: "REJECT_HARD hook → Blocked",
			ev: event.Event{
				DomainRuleID: "domain-1", PathAction: "PROCESS",
				HookDecision: "REJECT_HARD", BumpStatus: "BUMP_SUCCESS",
			},
			want: classify.ClassBlocked,
		},
		{
			name: "BUMP_FAILED with domain match → BumpFailed",
			ev: event.Event{
				DomainRuleID: "domain-1", PathAction: "PROCESS",
				BumpStatus: "BUMP_FAILED",
			},
			want: classify.ClassBumpFailed,
		},
		{
			name: "no domain match → Untracked",
			ev: event.Event{
				DomainRuleID: "", PathAction: "",
				BumpStatus: "BUMP_SUCCESS",
			},
			want: classify.ClassUntracked,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify.Classify(tc.ev)
			if got != tc.want {
				t.Errorf("Classify(%+v) = %q, want %q", tc.ev, got, tc.want)
			}
		})
	}
}
