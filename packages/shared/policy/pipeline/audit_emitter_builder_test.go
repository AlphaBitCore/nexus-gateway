package pipeline

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
)

// captureWriter records every Enqueue call so emit tests can assert
// the audit event was constructed.
type captureWriter struct {
	mu     sync.Mutex
	events []audit.AuditEvent
}

func (w *captureWriter) Enqueue(e audit.AuditEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, e)
}
func (w *captureWriter) Flush(context.Context) error { return nil }
func (w *captureWriter) Close(context.Context) error { return nil }
func (w *captureWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.events)
}

// nopSpill is a no-op SpillStore — wires the constructor branch
// without exercising spill IO.
type nopSpill struct{}

func (nopSpill) Put(context.Context, io.Reader, int64, spillstore.PutOptions) (audit.SpillRef, error) {
	return audit.SpillRef{}, nil
}
func (nopSpill) Get(context.Context, audit.SpillRef) (io.ReadCloser, error) {
	return io.NopCloser(nil), nil
}
func (nopSpill) Delete(context.Context, audit.SpillRef) error  { return nil }
func (nopSpill) Backend() string                               { return "test" }
func (nopSpill) Sweep(context.Context, time.Time) (int, error) { return 0, nil }
func (nopSpill) Stat(context.Context) (spillstore.Stats, error) {
	return spillstore.Stats{Backend: "test"}, nil
}
func (nopSpill) PresignPut(context.Context, string, int64, string, time.Duration) (string, error) {
	return "", nil
}
func (nopSpill) KeyFor(time.Time, string, string) string { return "" }

func testEmitterLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewAuditEmitter_AssignsFields(t *testing.T) {
	w := &captureWriter{}
	logger := testEmitterLogger()
	e := NewAuditEmitter(w, logger)
	if e == nil {
		t.Fatal("New returned nil")
		return
	}
	if e.writer == nil {
		t.Error("writer not assigned")
	}
	if e.logger == nil {
		t.Error("logger not assigned")
	}
	if e.spill != nil {
		t.Error("spill should be nil by default")
	}
	if e.payloadCapture != nil {
		t.Error("payloadCapture should be nil by default")
	}
}

func TestWithSpillStore_ChainableAndAssigns(t *testing.T) {
	e := NewAuditEmitter(&captureWriter{}, testEmitterLogger())
	// Chain returns the same receiver so callers can compose.
	got := e.WithSpillStore(nopSpill{})
	if got != e {
		t.Errorf("WithSpillStore should return the same receiver; got %p want %p", got, e)
	}
	if e.spill == nil {
		t.Error("spill was not assigned")
	}
}

func TestWithPayloadCaptureStore_ChainableAndAssigns(t *testing.T) {
	e := NewAuditEmitter(&captureWriter{}, testEmitterLogger())
	store := payloadcapture.NewStore(payloadcapture.DefaultConfig())
	got := e.WithPayloadCaptureStore(store)
	if got != e {
		t.Errorf("WithPayloadCaptureStore should return the same receiver")
	}
	if e.payloadCapture != store {
		t.Errorf("payloadCapture not assigned: got %p want %p", e.payloadCapture, store)
	}
}

func TestBuilder_ChainAllOptions(t *testing.T) {
	// Real usage: NewAuditEmitter(...).WithSpillStore(s).WithPayloadCaptureStore(p)
	w := &captureWriter{}
	store := payloadcapture.NewStore(payloadcapture.DefaultConfig())
	e := NewAuditEmitter(w, testEmitterLogger()).
		WithSpillStore(nopSpill{}).
		WithPayloadCaptureStore(store)
	if e.writer == nil || e.logger == nil || e.spill == nil || e.payloadCapture == nil {
		t.Errorf("chained build missing field: %+v", e)
	}
}

// Quiet "imported and not used" if captureWriter.count goes unused
// during future test additions.
var _ = (*captureWriter)(nil).count
