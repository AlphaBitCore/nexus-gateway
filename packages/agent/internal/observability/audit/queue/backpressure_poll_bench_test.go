package queue

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
)

var bpSink int

// newBenchQueue mirrors newWriterAdapterTestQueue, which takes *testing.T.
func newBenchQueue(b *testing.B) *Queue {
	b.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	q, err := NewQueue(filepath.Join(b.TempDir(), "audit.db"), key)
	if err != nil {
		b.Fatalf("NewQueue: %v", err)
	}
	b.Cleanup(func() { _ = q.Close() })
	return q
}

// Finding A-4's remainder, priced rather than argued.
//
// backpressure.Store.Poll wakes every 2 s and calls Queue.UnsyncedCount, which is
// SELECT COUNT(*) FROM audit_events WHERE synced = 0 against the encrypted queue —
// ~43,000 queries a day at idle. Making it event-driven would mean maintaining the
// unsynced count in memory across BOTH mutating paths (RecordBatch and the drain's
// mark-synced) and recovering it at restart: a new invariant on the audit-delivery
// path, where being wrong means either a missed throttle (audit events dropped while
// backpressure never engages) or a phantom one.
//
// This benchmark is what decided against that trade, and it is kept so the decision is
// reproducible rather than re-argued. Measured medians of 5 on an M-series laptop:
//
//	IdleEmpty        4.2 us  ->  0.18 s CPU/day at 30 polls/min
//	Backlog100       7.1 us  ->  0.31 s CPU/day
//	Backlog1000     23.2 us  ->  1.00 s CPU/day (and here the poll is doing real work)
//
// The idle case is nearly free because idx_audit_synced_created makes the synced = 0
// index prefix EMPTY when everything has been uploaded — the query touches almost
// nothing. 0.18 s of CPU per day does not justify a new correctness invariant on this
// path. Re-open A-4's remainder only if this benchmark's idle number moves materially,
// e.g. if the index is dropped or the predicate changes.
func benchUnsynced(b *testing.B, unsyncedRows int) {
	q := newBenchQueue(b)
	if unsyncedRows > 0 {
		batch := make([]event.Event, 0, unsyncedRows)
		for i := range unsyncedRows {
			batch = append(batch, event.Event{
				ID: fmt.Sprintf("bp-%d", i), Timestamp: time.Now().UTC(),
				TargetHost: "api.openai.com", Method: "POST", Path: "/v1/chat/completions",
			})
		}
		if err := q.RecordBatch(batch); err != nil {
			b.Fatalf("seed: %v", err)
		}
	}
	if got := q.UnsyncedCount(); got != unsyncedRows {
		b.Fatalf("seeded %d unsynced rows, UnsyncedCount reports %d", unsyncedRows, got)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		bpSink = q.UnsyncedCount()
	}
}

func BenchmarkUnsyncedCount_IdleEmpty(b *testing.B)   { benchUnsynced(b, 0) }
func BenchmarkUnsyncedCount_Backlog100(b *testing.B)  { benchUnsynced(b, 100) }
func BenchmarkUnsyncedCount_Backlog1000(b *testing.B) { benchUnsynced(b, 1000) }
