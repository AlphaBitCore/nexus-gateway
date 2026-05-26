package async

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
)

// fakeStore implements both SpillStore + Presigner so AsyncStore.New
// accepts it. PutDelay simulates S3 PutObject latency; PutErr forces a
// failure. Concurrency-safe so the uploader goroutine + test main
// goroutine can poll counters / observed keys without races.
type fakeStore struct {
	mu         sync.Mutex
	puts       []spillstore.PutOptions // observed Put calls (order = upload order)
	putDelay   time.Duration
	putErr     error
	gets       int32
	deletes    int32
	sweeps     int32
	stats      int32
}

func (f *fakeStore) Backend() string { return "fake" }
func (f *fakeStore) KeyFor(at time.Time, eventID, direction string) string {
	return fmt.Sprintf("%s/%s-%s.bin", at.Format("2006-01-02"), eventID, direction)
}
func (f *fakeStore) PresignPut(ctx context.Context, key string, sizeBytes int64, contentType string, expiresIn time.Duration) (string, error) {
	return "", spillstore.ErrPresignNotSupported
}
func (f *fakeStore) Put(ctx context.Context, content io.Reader, size int64, opts spillstore.PutOptions) (audit.SpillRef, error) {
	if f.putDelay > 0 {
		select {
		case <-time.After(f.putDelay):
		case <-ctx.Done():
			return audit.SpillRef{}, ctx.Err()
		}
	}
	if f.putErr != nil {
		return audit.SpillRef{}, f.putErr
	}
	// Drain the reader so the wrapper's buffered bytes are exercised.
	_, _ = io.Copy(io.Discard, content)
	f.mu.Lock()
	f.puts = append(f.puts, opts)
	f.mu.Unlock()
	return audit.SpillRef{Backend: f.Backend(), Key: f.KeyFor(time.Now().UTC(), opts.EventID, opts.Direction), Size: size, ContentType: opts.ContentType}, nil
}
func (f *fakeStore) Get(_ context.Context, _ audit.SpillRef) (io.ReadCloser, error) {
	atomic.AddInt32(&f.gets, 1)
	return io.NopCloser(strings.NewReader("ok")), nil
}
func (f *fakeStore) Delete(_ context.Context, _ audit.SpillRef) error {
	atomic.AddInt32(&f.deletes, 1)
	return nil
}
func (f *fakeStore) Sweep(_ context.Context, _ time.Time) (int, error) {
	atomic.AddInt32(&f.sweeps, 1)
	return 0, nil
}
func (f *fakeStore) Stat(_ context.Context) (spillstore.Stats, error) {
	atomic.AddInt32(&f.stats, 1)
	return spillstore.Stats{Backend: f.Backend()}, nil
}
func (f *fakeStore) observedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.puts))
	for _, p := range f.puts {
		out = append(out, p.EventID+"/"+p.Direction)
	}
	return out
}

// nonPresignerStore intentionally does NOT satisfy Presigner so we can
// verify New() rejects it.
type nonPresignerStore struct{ fakeStore }

func (n *nonPresignerStore) Backend() string { return "non-presigner" }

// We need to NOT inherit KeyFor / PresignPut from fakeStore. Easiest is
// to make nonPresignerStore stand alone.
type bareStore struct{}

func (b *bareStore) Backend() string { return "bare" }
func (b *bareStore) Put(_ context.Context, content io.Reader, size int64, opts spillstore.PutOptions) (audit.SpillRef, error) {
	_, _ = io.Copy(io.Discard, content)
	return audit.SpillRef{}, nil
}
func (b *bareStore) Get(_ context.Context, _ audit.SpillRef) (io.ReadCloser, error) {
	return nil, errors.New("bare get")
}
func (b *bareStore) Delete(_ context.Context, _ audit.SpillRef) error  { return nil }
func (b *bareStore) Sweep(_ context.Context, _ time.Time) (int, error) { return 0, nil }
func (b *bareStore) Stat(_ context.Context) (spillstore.Stats, error)  { return spillstore.Stats{}, nil }

func TestNew_RequiresInner(t *testing.T) {
	_, err := New(nil, Options{})
	if err == nil {
		t.Error("expected error when inner is nil")
	}
}

func TestNew_RequiresPresigner(t *testing.T) {
	_, err := New(&bareStore{}, Options{})
	if err == nil {
		t.Error("expected error when inner does not implement Presigner")
	}
	if !strings.Contains(err.Error(), "Presigner") {
		t.Errorf("error should mention Presigner; got: %v", err)
	}
}

func TestPut_ReturnsRefImmediately(t *testing.T) {
	inner := &fakeStore{putDelay: 200 * time.Millisecond}
	store, err := New(inner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	t0 := time.Now()
	ref, err := store.Put(context.Background(), strings.NewReader("hello"), 5, spillstore.PutOptions{
		EventID:     "ev-1",
		Direction:   "request",
		ContentType: "text/plain",
	})
	elapsed := time.Since(t0)
	if err != nil {
		t.Fatal(err)
	}
	// Put should return well under 200ms (the simulated upload delay).
	if elapsed > 50*time.Millisecond {
		t.Errorf("Put took %v (>50ms); should be near-zero — upload is supposed to be async", elapsed)
	}
	if ref.Backend != "fake" {
		t.Errorf("ref.Backend = %q, want fake", ref.Backend)
	}
	if ref.Key == "" {
		t.Errorf("ref.Key should be pre-computed, not empty")
	}
	if ref.Size != 5 {
		t.Errorf("ref.Size = %d, want 5", ref.Size)
	}
	if ref.SHA256 == "" {
		t.Errorf("ref.SHA256 should be computed inline")
	}
	if ref.ContentType != "text/plain" {
		t.Errorf("ref.ContentType = %q, want text/plain", ref.ContentType)
	}
}

func TestPut_RequiresEventID(t *testing.T) {
	store, err := New(&fakeStore{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	_, err = store.Put(context.Background(), strings.NewReader("x"), 1, spillstore.PutOptions{
		Direction: "request",
	})
	if err == nil || !strings.Contains(err.Error(), "EventID") {
		t.Errorf("expected EventID-required error; got %v", err)
	}
}

func TestPut_BackgroundUploadCompletes(t *testing.T) {
	inner := &fakeStore{}
	store, err := New(inner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	_, err = store.Put(context.Background(), strings.NewReader("payload"), 7, spillstore.PutOptions{
		EventID: "ev-2", Direction: "request",
	})
	if err != nil {
		t.Fatal(err)
	}

	flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.Flush(flushCtx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := store.Uploads(); got != 1 {
		t.Errorf("Uploads = %d, want 1", got)
	}
	if got := inner.observedKeys(); len(got) != 1 || got[0] != "ev-2/request" {
		t.Errorf("inner.observedKeys = %v, want [ev-2/request]", got)
	}
}

func TestPut_QueueFullDrops(t *testing.T) {
	// Tiny queue (capacity=1) + slow inner Put → second Put hits "queue full".
	inner := &fakeStore{putDelay: 500 * time.Millisecond}
	store, err := New(inner, Options{QueueCapacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	// First Put: queued. Second: queue full (worker still in 500ms sleep).
	// Third+: also full.
	for i := 0; i < 5; i++ {
		_, err := store.Put(context.Background(), strings.NewReader("x"), 1, spillstore.PutOptions{
			EventID: fmt.Sprintf("ev-%d", i), Direction: "request",
		})
		if err != nil {
			t.Errorf("Put %d: %v", i, err)
		}
	}
	// At least 3 should be dropped (1 in queue + 1 in-flight = 2 fit; 5-2=3).
	// Tighter check than == not needed; range is fine.
	if d := store.Drops(); d < 3 {
		t.Errorf("Drops = %d, want >= 3", d)
	}
}

func TestPut_UploadFailureCounted(t *testing.T) {
	inner := &fakeStore{putErr: errors.New("S3 exploded")}
	store, err := New(inner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	_, err = store.Put(context.Background(), strings.NewReader("x"), 1, spillstore.PutOptions{
		EventID: "ev-fail", Direction: "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	flushCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := store.Flush(flushCtx); err != nil {
		t.Fatal(err)
	}
	if got := store.Failures(); got != 1 {
		t.Errorf("Failures = %d, want 1", got)
	}
	if got := store.Uploads(); got != 0 {
		t.Errorf("Uploads = %d, want 0", got)
	}
}

func TestPut_ContentTypeDefault(t *testing.T) {
	inner := &fakeStore{}
	store, _ := New(inner, Options{})
	defer func() { _ = store.Close(context.Background()) }()

	ref, _ := store.Put(context.Background(), strings.NewReader("x"), 1, spillstore.PutOptions{
		EventID: "ev-default-ct", Direction: "request",
	})
	if ref.ContentType != "application/octet-stream" {
		t.Errorf("ref.ContentType = %q, want application/octet-stream", ref.ContentType)
	}
}

func TestPut_ContentReadError(t *testing.T) {
	store, _ := New(&fakeStore{}, Options{})
	defer func() { _ = store.Close(context.Background()) }()

	_, err := store.Put(context.Background(), &errReader{}, 100, spillstore.PutOptions{
		EventID: "ev-readerr", Direction: "request",
	})
	if err == nil {
		t.Error("expected read error")
	}
}

type errReader struct{}

func (e *errReader) Read(_ []byte) (int, error) { return 0, errors.New("read boom") }

func TestPassthrough_GetDeleteSweepStat(t *testing.T) {
	inner := &fakeStore{}
	store, _ := New(inner, Options{})
	defer func() { _ = store.Close(context.Background()) }()

	if _, err := store.Get(context.Background(), audit.SpillRef{}); err != nil {
		t.Errorf("Get: %v", err)
	}
	if err := store.Delete(context.Background(), audit.SpillRef{}); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if _, err := store.Sweep(context.Background(), time.Now()); err != nil {
		t.Errorf("Sweep: %v", err)
	}
	if _, err := store.Stat(context.Background()); err != nil {
		t.Errorf("Stat: %v", err)
	}
	if atomic.LoadInt32(&inner.gets) != 1 {
		t.Errorf("inner.gets = %d, want 1", inner.gets)
	}
}

func TestBackend_MirrorsInner(t *testing.T) {
	store, _ := New(&fakeStore{}, Options{})
	defer func() { _ = store.Close(context.Background()) }()
	if got := store.Backend(); got != "fake" {
		t.Errorf("Backend() = %q, want fake", got)
	}
}

func TestClose_Idempotent(t *testing.T) {
	store, _ := New(&fakeStore{}, Options{})
	if err := store.Close(context.Background()); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := store.Close(context.Background()); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestClose_DrainsQueue(t *testing.T) {
	inner := &fakeStore{putDelay: 50 * time.Millisecond}
	store, _ := New(inner, Options{})
	for i := 0; i < 5; i++ {
		_, _ = store.Put(context.Background(), strings.NewReader("x"), 1, spillstore.PutOptions{
			EventID: fmt.Sprintf("ev-%d", i), Direction: "request",
		})
	}
	// Close should drain all queued before returning.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := store.Uploads(); got != 5 {
		t.Errorf("Uploads after Close = %d, want 5", got)
	}
}

func TestClose_TimesOutWhenWorkerStuck(t *testing.T) {
	// Worker stuck in 2s Put + 100ms drain timeout = should time out.
	inner := &fakeStore{putDelay: 2 * time.Second}
	store, _ := New(inner, Options{DrainTimeout: 100 * time.Millisecond})
	_, _ = store.Put(context.Background(), strings.NewReader("x"), 1, spillstore.PutOptions{
		EventID: "ev-stuck", Direction: "request",
	})
	// give the worker a moment to pick up the job
	time.Sleep(10 * time.Millisecond)
	err := store.Close(context.Background())
	if err == nil {
		t.Error("expected drain timeout error")
	}
}

func TestFlush_BeforeAnyPuts(t *testing.T) {
	store, _ := New(&fakeStore{}, Options{})
	defer func() { _ = store.Close(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := store.Flush(ctx); err != nil {
		t.Errorf("Flush: %v", err)
	}
}

func TestFlush_AfterClose(t *testing.T) {
	store, _ := New(&fakeStore{}, Options{})
	if err := store.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Flush on a closed store returns nil (drained on its own).
	if err := store.Flush(context.Background()); err != nil {
		t.Errorf("Flush after Close: %v", err)
	}
}

func TestFlush_CtxCancelled(t *testing.T) {
	// Big queue + slow Put so Flush can't drain in time; cancelled ctx
	// should surface ctx.Err().
	inner := &fakeStore{putDelay: 1 * time.Second}
	store, _ := New(inner, Options{QueueCapacity: 16})
	defer func() { _ = store.Close(context.Background()) }()

	for i := 0; i < 5; i++ {
		_, _ = store.Put(context.Background(), strings.NewReader("x"), 1, spillstore.PutOptions{
			EventID: fmt.Sprintf("ev-%d", i), Direction: "request",
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Flush(ctx); err == nil {
		t.Error("expected ctx-cancelled error from Flush")
	}
}

func TestNilStore_PutAndFlushCloseSafe(t *testing.T) {
	// Defensive: Flush/Close on a nil AsyncStore is a no-op (mirrors how
	// other agent components handle nil receivers).
	var a *AsyncStore
	if err := a.Flush(context.Background()); err != nil {
		t.Errorf("Flush nil: %v", err)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Errorf("Close nil: %v", err)
	}
}

func TestPut_SizeHintZero_StillReadsContent(t *testing.T) {
	// size=0 hint with non-empty reader — buf.Grow takes the zero, then
	// io.Copy still drains. Returned ref carries the actually-read size.
	inner := &fakeStore{}
	store, _ := New(inner, Options{})
	defer func() { _ = store.Close(context.Background()) }()

	ref, err := store.Put(context.Background(), strings.NewReader("hidden"), 0, spillstore.PutOptions{
		EventID: "ev-zero-hint", Direction: "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ref.Size != 6 {
		t.Errorf("ref.Size = %d, want 6", ref.Size)
	}
}

func TestConcurrentPuts_AllUploaded(t *testing.T) {
	inner := &fakeStore{}
	store, _ := New(inner, Options{QueueCapacity: 1024})
	defer func() { _ = store.Close(context.Background()) }()

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := bytes.Repeat([]byte("x"), 16)
			_, _ = store.Put(context.Background(), bytes.NewReader(body), int64(len(body)), spillstore.PutOptions{
				EventID: fmt.Sprintf("ev-conc-%d", i), Direction: "request",
			})
		}(i)
	}
	wg.Wait()
	flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.Flush(flushCtx); err != nil {
		t.Fatal(err)
	}
	if got := store.Uploads(); got != n {
		t.Errorf("Uploads = %d, want %d", got, n)
	}
}
