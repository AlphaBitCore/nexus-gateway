package queue

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
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
