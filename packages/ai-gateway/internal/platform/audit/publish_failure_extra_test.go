package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	sharedndjson "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/ndjson"
)

// errFramePublish is the injected publish failure for the framed-path tests.
var errFramePublish = errors.New("frame publish boom")

// failBatchProducer is a pooled batchProducer whose every frame publish fails —
// exercises the FRAMED failure path (publishFramed routes every record in a failed
// frame through handlePublishFailure), the path the memProducer (per-record) tests
// do not cover.
type failBatchProducer struct{}

func (failBatchProducer) Publish(context.Context, string, []byte) error { return errFramePublish }
func (failBatchProducer) Enqueue(context.Context, string, []byte) error { return errFramePublish }
func (failBatchProducer) Close() error                                  { return nil }
func (failBatchProducer) PoolSize() int                                 { return 1 }

func (failBatchProducer) EnqueueBatchAsync(_ context.Context, _ string, b [][]byte) ([]error, error) {
	errs := make([]error, len(b))
	for i := range errs {
		errs[i] = errFramePublish
	}
	return errs, nil
}

func (failBatchProducer) EnqueueBatchAsyncOn(_ context.Context, _ string, b [][]byte, _ int) ([]error, error) {
	errs := make([]error, len(b))
	for i := range errs {
		errs[i] = errFramePublish
	}
	return errs, nil
}

// TestPublishFailure_SpillBlockNoSpool_DowngradesToBlock pins the no-loss fix for
// the block→spillBlock default change: spillBlock uses the durable spool as its
// overflow buffer, so WITHOUT a spool wired (empty spoolDir OR a spool-dir creation
// failure) it must downgrade to block at Start — the stricter no-loss mode that
// back-pressures at the in-heap queue and needs no spool — rather than dropping on
// the first overflow (which spillOverflow's no-sink branch would do).
func TestPublishFailure_SpillBlockNoSpool_DowngradesToBlock(t *testing.T) {
	// spillBlock selected, but NO WithNDJSONSpill → ndjsonSpill stays nil.
	w := NewWriter(&memProducer{}, "nexus.event.ai-traffic", nil, slog.Default()).
		WithLossMode(lossModeSpillBlock)
	if got := w.LossMode(); got != lossModeSpillBlock {
		t.Fatalf("pre-Start lossMode=%q want %q", got, lossModeSpillBlock)
	}
	w.Start()
	defer w.Close()
	if got := w.LossMode(); got != lossModeBlock {
		t.Errorf("spillBlock without a spool must downgrade to block at Start (no-loss "+
			"back-pressure, not first-overflow drop); got %q", got)
	}
}

// TestPublishFailure_SpillBlockWithSpool_Kept verifies the downgrade is conditional:
// with a spool wired, spillBlock is kept (the spool is its no-loss overflow buffer).
func TestPublishFailure_SpillBlockWithSpool_Kept(t *testing.T) {
	dir := t.TempDir()
	spill, err := sharedndjson.New(dir, "test", 4096, 64<<20, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	w := NewWriter(&memProducer{}, "nexus.event.ai-traffic", nil, slog.Default()).
		WithLossMode(lossModeSpillBlock).WithNDJSONSpill(spill).Start()
	defer w.Close()
	if got := w.LossMode(); got != lossModeSpillBlock {
		t.Errorf("spillBlock with a spool wired must be kept; got %q", got)
	}
}

// TestPublishFailure_FramedPath_Bounded covers the FRAMED failure path: a batch
// producer whose every frame publish fails routes every record through
// handlePublishFailure, which must bound the retry and spill durably — no infinite
// circulation, no loss — exactly like the per-record path.
func TestPublishFailure_FramedPath_Bounded(t *testing.T) {
	dir := t.TempDir()
	spill, err := sharedndjson.New(dir, "test", 4096, 64<<20, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	w := NewWriter(failBatchProducer{}, "nexus.event.ai-traffic", nil, slog.Default()).
		WithFramePublish(256 << 10). // force the framed publish path
		WithNDJSONSpill(spill).Start()
	defer w.Close()

	const n = 50
	for i := range n {
		w.Enqueue(&Record{RequestID: fmt.Sprintf("frame-stuck-%d", i), Timestamp: time.Now().Add(time.Duration(i))})
	}

	deadline := time.Now().Add(5 * time.Second)
	for len(readSpool(t, dir, "test")) < n {
		if time.Now().After(deadline) {
			t.Fatalf("framed sustained-outage records must all spill durably (bounded); got %d, want %d",
				len(readSpool(t, dir, "test")), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// BenchmarkHandlePublishFailure_SpillRouting proves the wedged-outage routing does
// NOT become a new bottleneck (the user's explicit constraint: the fix must not turn
// into a choke-point). It drives handlePublishFailure from N parallel workers with a
// spool PRE-POPULATED with many sealed files (so the per-record spillData path's
// O(spool-files) dirSize cost is realistic), comparing the two routings:
//
//	spillch  — the fix: cap-exhausted records hand off to the batched async spill
//	           worker (spillCh → WriteBatch, one dirSize per batch, off the caller).
//	spilldata — the pre-fix per-record path: synchronous ndjson.Write under a single
//	           mutex + a dirSize (readDir + stat every spool file) PER record.
//
// The spillch arm should stay flat as -cpu (contention) rises while the spilldata
// arm degrades — the empirical "not a bottleneck" evidence the design calls for.
//
//	go test -run x -bench BenchmarkHandlePublishFailure_SpillRouting -cpu 1,8,32 \
//	  -benchmem ./internal/platform/store/... (audit pkg)
func BenchmarkHandlePublishFailure_SpillRouting(b *testing.B) {
	mkWriter := func(b *testing.B) (*Writer, string) {
		dir := b.TempDir()
		// Small per-file cap so a modest volume seals MANY files → realistic dirSize cost.
		spill, err := sharedndjson.New(dir, "bench", 4096, 1<<30, nil)
		if err != nil {
			b.Fatalf("ndjson.New: %v", err)
		}
		// Pre-seal ~150 files so dirSize (readDir + stat-per-file) is non-trivial.
		for i := range 150 * 4096 / 64 {
			_ = spill.Write([]byte(fmt.Sprintf("seed-%d\n", i)))
		}
		w := NewWriter(&memProducer{}, "nexus.event.ai-traffic", nil,
			slog.New(slog.NewTextHandler(discardWriter{}, nil))).
			WithNDJSONSpill(spill).Start()
		return w, dir
	}

	b.Run("spillch", func(b *testing.B) {
		w, _ := mkWriter(b)
		defer w.Close()
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				rec := &Record{RequestID: "x", Timestamp: time.Now(), publishRetries: maxPublishRetries + 1}
				rec.marshaled = []byte(`{"requestId":"x"}`)
				w.handlePublishFailure(rec.marshaled, rec) // cap already exceeded → spillCh
			}
		})
	})

	b.Run("spilldata", func(b *testing.B) {
		w, _ := mkWriter(b)
		defer w.Close()
		data := []byte(`{"requestId":"x"}`)
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				w.spillData(data) // the pre-fix per-record synchronous path
			}
		})
	})
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
