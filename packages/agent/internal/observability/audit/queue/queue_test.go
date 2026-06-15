package queue

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

func newTestQueue(t *testing.T) *Queue {
	t.Helper()
	q, err := NewQueue(":memory:", nil)
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func makeEvent(id string) event.Event {
	return event.Event{
		ID: id, Timestamp: time.Now(), SourceProcess: "/usr/bin/curl",
		TargetHost: "api.openai.com", DestIP: "1.2.3.4", DestPort: 443, Action: "inspect",
	}
}

func TestRecord_InsertsUnsynced(t *testing.T) {
	q := newTestQueue(t)
	if err := q.Record(makeEvent("e1")); err != nil {
		t.Fatalf("record failed: %v", err)
	}
	if q.UnsyncedCount() != 1 {
		t.Errorf("expected 1 unsynced, got %d", q.UnsyncedCount())
	}
}

func TestDrainBatch_ReturnsUnsynced(t *testing.T) {
	q := newTestQueue(t)
	for i := range 5 {
		_ = q.Record(makeEvent(fmt.Sprintf("e%d", i)))
	}
	events, err := q.DrainBatch(3)
	if err != nil {
		t.Fatalf("drain failed: %v", err)
	}
	if len(events) != 3 {
		t.Errorf("expected 3, got %d", len(events))
	}
}

func TestMarkSynced_RemovesFromDrain(t *testing.T) {
	q := newTestQueue(t)
	_ = q.Record(makeEvent("e1"))
	_ = q.Record(makeEvent("e2"))

	if err := q.MarkSynced([]string{"e1"}); err != nil {
		t.Fatalf("mark synced failed: %v", err)
	}

	if q.UnsyncedCount() != 1 {
		t.Errorf("expected 1 unsynced after marking e1, got %d", q.UnsyncedCount())
	}
}

func TestDrainFailure_KeepsUnsynced(t *testing.T) {
	q := newTestQueue(t)
	_ = q.Record(makeEvent("e1"))

	// Drain but don't mark synced (simulating upload failure)
	events, _ := q.DrainBatch(10)
	if len(events) != 1 {
		t.Fatalf("expected 1 event")
	}
	// Don't call MarkSynced — events should still be unsynced
	if q.UnsyncedCount() != 1 {
		t.Errorf("events should remain unsynced on drain failure")
	}
}

func TestPruneSynced_DeletesOld(t *testing.T) {
	q := newTestQueue(t)
	_ = q.Record(makeEvent("e1"))
	_ = q.MarkSynced([]string{"e1"})

	deleted, err := q.PruneSynced(0)
	if err != nil {
		t.Fatalf("prune failed: %v", err)
	}
	if deleted != 1 {
		t.Errorf("expected 1 deleted, got %d", deleted)
	}
}

func TestPruneSynced_KeepsUnsynced(t *testing.T) {
	q := newTestQueue(t)
	_ = q.Record(makeEvent("e1"))

	deleted, err := q.PruneSynced(0)
	if err != nil {
		t.Fatalf("prune failed: %v", err)
	}
	if deleted != 0 {
		t.Errorf("expected 0 deleted (unsynced preserved), got %d", deleted)
	}
	if q.UnsyncedCount() != 1 {
		t.Error("unsynced event should survive prune")
	}
}

func TestConcurrentRecords(t *testing.T) {
	q := newTestQueue(t)
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = q.Record(makeEvent(fmt.Sprintf("c%d", n)))
		}(i)
	}
	wg.Wait()
	if q.UnsyncedCount() != 10 {
		t.Errorf("expected 10 from concurrent writes, got %d", q.UnsyncedCount())
	}
}

func TestDuplicateInsert_Ignored(t *testing.T) {
	q := newTestQueue(t)
	_ = q.Record(makeEvent("e1"))
	_ = q.Record(makeEvent("e1")) // duplicate
	if q.UnsyncedCount() != 1 {
		t.Errorf("duplicate should be ignored, got %d", q.UnsyncedCount())
	}
}

func TestDrainOnce_SuccessfulUpload(t *testing.T) {
	q := newTestQueue(t)
	_ = q.Record(makeEvent("e1"))
	_ = q.Record(makeEvent("e2"))

	var uploaded []event.Event
	q.drainOnce(10, func(events []event.Event) error {
		uploaded = events
		return nil
	})

	if len(uploaded) != 2 {
		t.Errorf("expected 2 uploaded, got %d", len(uploaded))
	}
	if q.UnsyncedCount() != 0 {
		t.Errorf("all events should be synced after successful upload, got %d unsynced", q.UnsyncedCount())
	}
}

func TestDrainOnce_FailedUpload_KeepsUnsynced(t *testing.T) {
	q := newTestQueue(t)
	_ = q.Record(makeEvent("e1"))

	q.drainOnce(10, func(events []event.Event) error {
		return fmt.Errorf("network error")
	})

	if q.UnsyncedCount() != 1 {
		t.Errorf("events should remain unsynced on upload failure, got %d", q.UnsyncedCount())
	}
}

func TestDrainOnce_EmptyQueue(t *testing.T) {
	q := newTestQueue(t)
	called := false
	q.drainOnce(10, func(events []event.Event) error {
		called = true
		return nil
	})
	if called {
		t.Error("upload should not be called on empty queue")
	}
}

func TestMarkSynced_EmptyList(t *testing.T) {
	q := newTestQueue(t)
	err := q.MarkSynced([]string{})
	if err != nil {
		t.Errorf("marking empty list should not error: %v", err)
	}
}

func TestQueryEvents_NoFilter(t *testing.T) {
	q := newTestQueue(t)
	for i := range 20 {
		_ = q.Record(event.Event{
			ID: fmt.Sprintf("q%d", i), Timestamp: time.Now(),
			SourceProcess: fmt.Sprintf("app%d", i%3), TargetHost: fmt.Sprintf("api%d.example.com", i%4),
			DestIP: "1.2.3.4", DestPort: 443, Action: []string{"inspect", "passthrough", "deny"}[i%3],
		})
	}
	events, total, err := q.QueryEvents("", "", 0, 10)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if total != 20 {
		t.Errorf("expected total 20, got %d", total)
	}
	if len(events) != 10 {
		t.Errorf("expected 10 events (limit), got %d", len(events))
	}
}

func TestQueryEvents_SearchByProcess(t *testing.T) {
	q := newTestQueue(t)
	_ = q.Record(event.Event{ID: "s1", Timestamp: time.Now(), SourceProcess: "/usr/bin/curl", TargetHost: "api.openai.com", DestIP: "1.1.1.1", DestPort: 443, Action: "inspect"})
	_ = q.Record(event.Event{ID: "s2", Timestamp: time.Now(), SourceProcess: "/Applications/Cursor.app", TargetHost: "api.anthropic.com", DestIP: "2.2.2.2", DestPort: 443, Action: "inspect"})

	events, total, err := q.QueryEvents("curl", "", 0, 50)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if total != 1 {
		t.Errorf("expected 1 match for 'curl', got %d", total)
	}
	if len(events) != 1 || events[0].ID != "s1" {
		t.Error("expected s1")
	}
}

func TestQueryEvents_FilterByAction(t *testing.T) {
	q := newTestQueue(t)
	_ = q.Record(event.Event{ID: "a1", Timestamp: time.Now(), SourceProcess: "p", TargetHost: "h", DestIP: "1.1.1.1", DestPort: 443, Action: "inspect"})
	_ = q.Record(event.Event{ID: "a2", Timestamp: time.Now(), SourceProcess: "p", TargetHost: "h", DestIP: "1.1.1.1", DestPort: 443, Action: "deny"})

	events, total, err := q.QueryEvents("", "deny", 0, 50)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if total != 1 || events[0].ID != "a2" {
		t.Errorf("expected 1 deny event, got %d", total)
	}
}

func TestQueryEvents_Pagination(t *testing.T) {
	q := newTestQueue(t)
	for i := range 10 {
		_ = q.Record(event.Event{ID: fmt.Sprintf("p%d", i), Timestamp: time.Now().Add(time.Duration(i) * time.Second), SourceProcess: "p", TargetHost: "h", DestIP: "1.1.1.1", DestPort: 443, Action: "inspect"})
	}
	events, _, _ := q.QueryEvents("", "", 5, 3)
	if len(events) != 3 {
		t.Errorf("expected 3 events at offset 5, got %d", len(events))
	}
}

func TestRecord_PayloadBodiesRoundtrip(t *testing.T) {
	// Bodies stamped onto audit.Event.PayloadRequest /
	// PayloadResponse must persist into the payload_request /
	// payload_response BLOB columns and come back unchanged through
	// DrainBatch so the upload path can forward them to Hub.
	q := newTestQueue(t)

	reqBody := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	respBody := []byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hello"}}]}`)

	e := makeEvent("pc-1")
	e.PayloadRequest = reqBody
	e.PayloadResponse = respBody
	if err := q.Record(e); err != nil {
		t.Fatalf("record failed: %v", err)
	}

	got, err := q.DrainBatch(10)
	if err != nil {
		t.Fatalf("drain failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("drain returned %d events, want 1", len(got))
	}
	if string(got[0].PayloadRequest) != string(reqBody) {
		t.Errorf("request roundtrip mismatch:\n got %q\nwant %q",
			got[0].PayloadRequest, reqBody)
	}
	if string(got[0].PayloadResponse) != string(respBody) {
		t.Errorf("response roundtrip mismatch:\n got %q\nwant %q",
			got[0].PayloadResponse, respBody)
	}
}

func TestRecord_SpillRefsRoundtrip(t *testing.T) {
	// Oversize bodies spilled to the local store keep only a SpillRef on the
	// audit row. The ref (JSON-encoded) must persist into request_spill_ref /
	// response_spill_ref and decode back unchanged through DrainBatch so the
	// drain step can read the local body and upload it to S3.
	q := newTestQueue(t)

	e := makeEvent("spill-1")
	e.RequestSpillRef = &sharedaudit.SpillRef{
		Backend: "localfs", Key: "2026-05-27/spill-1-request.bin",
		Size: 700000, SHA256: "deadbeef", ContentType: "application/json",
	}
	e.ResponseSpillRef = &sharedaudit.SpillRef{
		Backend: "localfs", Key: "2026-05-27/spill-1-response.bin", Size: 900000,
	}
	if err := q.Record(e); err != nil {
		t.Fatalf("record failed: %v", err)
	}

	got, err := q.DrainBatch(10)
	if err != nil {
		t.Fatalf("drain failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("drain returned %d events, want 1", len(got))
	}
	if got[0].RequestSpillRef == nil || got[0].ResponseSpillRef == nil {
		t.Fatalf("spill refs lost on roundtrip: req=%v resp=%v",
			got[0].RequestSpillRef, got[0].ResponseSpillRef)
	}
	if got[0].RequestSpillRef.Key != e.RequestSpillRef.Key ||
		got[0].RequestSpillRef.Backend != "localfs" ||
		got[0].RequestSpillRef.Size != 700000 ||
		got[0].RequestSpillRef.ContentType != "application/json" {
		t.Errorf("request spill ref roundtrip mismatch: got %+v want %+v",
			got[0].RequestSpillRef, e.RequestSpillRef)
	}
	if got[0].ResponseSpillRef.Key != e.ResponseSpillRef.Key {
		t.Errorf("response spill ref key mismatch: got %q want %q",
			got[0].ResponseSpillRef.Key, e.ResponseSpillRef.Key)
	}
	// Inline body columns stay empty when the body spilled.
	if len(got[0].PayloadRequest) != 0 || len(got[0].PayloadResponse) != 0 {
		t.Errorf("spill path should leave inline payload empty")
	}
}

func TestEventByID_FullDetail(t *testing.T) {
	q := newTestQueue(t)
	pt, ct := 11, 22
	ttfb, total, rh, resph := 30, 120, 5, 8
	e := makeEvent("detail-1")
	e.Method = http.MethodPost
	e.Path = "/v1/chat/completions"
	e.StatusCode = 200
	e.ProviderName = "openai"
	e.ModelName = "gpt-4o"
	e.ApiKeyClass = "personal"
	e.ApiKeyFingerprint = "fp-1"
	e.UsageExtractionStatus = "ok"
	e.PromptTokens = &pt
	e.CompletionTokens = &ct
	e.ComplianceTags = []string{"detector:pii"}
	e.DomainRuleID = "dom-1"
	e.PathAction = "PROCESS"
	e.UpstreamTtfbMs = &ttfb
	e.UpstreamTotalMs = &total
	e.RequestHooksMs = &rh
	e.ResponseHooksMs = &resph
	e.LatencyBreakdown = map[string]int{"intercept_ms": 2}
	e.HooksPipeline = json.RawMessage(`[{"hook":"pii"}]`)
	e.PayloadRequest = []byte("inline req body")
	e.NormalizedRequest = json.RawMessage(`{"model":"gpt-4o"}`)
	e.NormalizedResponse = json.RawMessage(`{"id":"c-1"}`)
	e.ResponseSpillRef = &sharedaudit.SpillRef{Backend: "localfs", Key: "d/detail-1-response.bin", Size: 900000}
	if err := q.Record(e); err != nil {
		t.Fatalf("record failed: %v", err)
	}
	got, err := q.EventByID("detail-1")
	if err != nil {
		t.Fatalf("EventByID failed: %v", err)
	}
	if got == nil {
		t.Fatal("EventByID returned nil for an existing id")
	}
	if got.Method != http.MethodPost || got.Path != "/v1/chat/completions" || got.StatusCode != http.StatusOK {
		t.Errorf("request line mismatch: %s %s %d", got.Method, got.Path, got.StatusCode)
	}
	if got.PromptTokens == nil || *got.PromptTokens != 11 || got.CompletionTokens == nil || *got.CompletionTokens != 22 {
		t.Errorf("token roundtrip mismatch: %v %v", got.PromptTokens, got.CompletionTokens)
	}
	if got.UpstreamTtfbMs == nil || got.UpstreamTotalMs == nil || got.RequestHooksMs == nil || got.ResponseHooksMs == nil {
		t.Error("latency pointers should survive detail roundtrip")
	}
	if got.LatencyBreakdown["intercept_ms"] != 2 {
		t.Errorf("latency breakdown mismatch: %v", got.LatencyBreakdown)
	}
	if len(got.HooksPipeline) == 0 || len(got.ComplianceTags) != 1 {
		t.Errorf("hooks pipeline / tags lost: %q %v", got.HooksPipeline, got.ComplianceTags)
	}
	if got.DomainRuleID != "dom-1" || got.PathAction != "PROCESS" {
		t.Errorf("classification inputs lost: %s %s", got.DomainRuleID, got.PathAction)
	}
	if string(got.PayloadRequest) != "inline req body" {
		t.Errorf("inline body mismatch: %q", got.PayloadRequest)
	}
	if string(got.NormalizedRequest) != `{"model":"gpt-4o"}` || string(got.NormalizedResponse) != `{"id":"c-1"}` {
		t.Errorf("normalized mismatch: %q %q", got.NormalizedRequest, got.NormalizedResponse)
	}
	if got.ResponseSpillRef == nil || got.ResponseSpillRef.Key != "d/detail-1-response.bin" {
		t.Errorf("spill ref not returned in detail: %+v", got.ResponseSpillRef)
	}
}

func TestEventByID_NotFound(t *testing.T) {
	q := newTestQueue(t)
	got, err := q.EventByID("does-not-exist")
	if err != nil {
		t.Fatalf("EventByID should not error on unknown id: %v", err)
	}
	if got != nil {
		t.Errorf("EventByID should return nil for unknown id, got %+v", got)
	}
}

func TestRecord_NoSpillRefStoresNULL(t *testing.T) {
	// Inline / no-capture rows must leave the spill columns NULL → decode to
	// nil so the drain step skips the S3 upload path entirely.
	q := newTestQueue(t)
	if err := q.Record(makeEvent("nospill")); err != nil {
		t.Fatalf("record failed: %v", err)
	}
	got, err := q.DrainBatch(10)
	if err != nil {
		t.Fatalf("drain failed: %v", err)
	}
	if got[0].RequestSpillRef != nil || got[0].ResponseSpillRef != nil {
		t.Errorf("no-spill row should decode to nil refs, got req=%v resp=%v",
			got[0].RequestSpillRef, got[0].ResponseSpillRef)
	}
}

func TestRecord_EmptyPayloadsStoreAsNULL(t *testing.T) {
	// When capture is disabled, PayloadRequest / PayloadResponse are nil
	// and nullableBytes keeps the row free of BLOB overhead. DrainBatch
	// must surface those as nil (not empty []byte{}) so auditEventToMap
	// can use the len() > 0 omit-when-empty convention consistently.
	q := newTestQueue(t)
	if err := q.Record(makeEvent("pc-empty")); err != nil {
		t.Fatalf("record failed: %v", err)
	}
	got, err := q.DrainBatch(10)
	if err != nil {
		t.Fatalf("drain failed: %v", err)
	}
	if len(got[0].PayloadRequest) != 0 {
		t.Errorf("empty capture should surface as nil/empty; got %q", got[0].PayloadRequest)
	}
	if len(got[0].PayloadResponse) != 0 {
		t.Errorf("empty capture should surface as nil/empty; got %q", got[0].PayloadResponse)
	}
}

func TestNewQueue_TempFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/test-audit.db"
	q, err := NewQueue(dbPath, nil)
	if err != nil {
		t.Fatalf("failed to create file-based queue: %v", err)
	}
	defer q.Close() //nolint:errcheck

	_ = q.Record(makeEvent("f1"))
	if q.UnsyncedCount() != 1 {
		t.Errorf("expected 1 unsynced in file-based queue, got %d", q.UnsyncedCount())
	}
}

// TestRedactionSpansRoundtrip — the storage-governed redaction spans must
// survive every SQLite round-trip (single insert, batch insert, drain for
// upload, detail-view lookup), and unredacted rows must read back nil so
// the upload map omits the keys entirely.
func TestRedactionSpansRoundtrip(t *testing.T) {
	q := newTestQueue(t)
	reqSpans := `[{"start":8,"end":24,"replacement":"[EMAIL-REDACTED]","contentAddress":"messages.0.content.0"}]`
	respSpans := `[{"start":0,"end":4,"replacement":"[X]","contentAddress":"messages.1.content.0"}]`

	redacted := makeEvent("spans-1")
	redacted.RequestRedactionSpans = json.RawMessage(reqSpans)
	redacted.ResponseRedactionSpans = json.RawMessage(respSpans)
	if err := q.Record(redacted); err != nil {
		t.Fatalf("record: %v", err)
	}

	plain := makeEvent("spans-2")
	if err := q.RecordBatch([]event.Event{plain}); err != nil {
		t.Fatalf("record batch: %v", err)
	}

	drained, err := q.DrainBatch(10)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	byID := map[string]event.Event{}
	for _, e := range drained {
		byID[e.ID] = e
	}
	got := byID["spans-1"]
	if string(got.RequestRedactionSpans) != reqSpans {
		t.Errorf("drained request spans = %q, want %q", got.RequestRedactionSpans, reqSpans)
	}
	if string(got.ResponseRedactionSpans) != respSpans {
		t.Errorf("drained response spans = %q, want %q", got.ResponseRedactionSpans, respSpans)
	}
	if byID["spans-2"].RequestRedactionSpans != nil || byID["spans-2"].ResponseRedactionSpans != nil {
		t.Errorf("unredacted row must read back nil spans")
	}

	detail, err := q.EventByID("spans-1")
	if err != nil || detail == nil {
		t.Fatalf("EventByID: %v %v", detail, err)
	}
	if string(detail.RequestRedactionSpans) != reqSpans || string(detail.ResponseRedactionSpans) != respSpans {
		t.Errorf("detail spans = %q / %q", detail.RequestRedactionSpans, detail.ResponseRedactionSpans)
	}
}
