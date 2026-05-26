package spillstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// fakeStore implements SpillStore for tests; controls Put behaviour.
type fakeStore struct {
	putCalls int
	failPut  bool
}

func (f *fakeStore) Put(_ context.Context, _ io.Reader, size int64, opts PutOptions) (audit.SpillRef, error) {
	f.putCalls++
	if f.failPut {
		return audit.SpillRef{}, errors.New("backend exploded")
	}
	return audit.SpillRef{Backend: "fake", Key: "k", Size: size, ContentType: opts.ContentType}, nil
}
func (f *fakeStore) Get(_ context.Context, _ audit.SpillRef) (io.ReadCloser, error) {
	return nil, ErrNotFound
}
func (f *fakeStore) Delete(_ context.Context, _ audit.SpillRef) error  { return nil }
func (f *fakeStore) Sweep(_ context.Context, _ time.Time) (int, error) { return 0, nil }
func (f *fakeStore) Stat(_ context.Context) (Stats, error)             { return Stats{}, nil }
func (f *fakeStore) Backend() string                                   { return "fake" }

func TestEmitBody_AbsentOnEmpty(t *testing.T) {
	b := EmitBody(context.Background(), nil, 1024, nil, "", "id1", "request", false, nil)
	if b.Kind != "absent" {
		t.Errorf("Kind = %q, want absent", b.Kind)
	}
}

func TestEmitBody_InlineWhenBelowThreshold(t *testing.T) {
	store := &fakeStore{}
	body := make([]byte, 100)
	b := EmitBody(context.Background(), store, 1024, body, "application/json", "id1", "request", false, nil)
	if b.Kind != "inline" {
		t.Errorf("Kind = %q, want inline", b.Kind)
	}
	if store.putCalls != 0 {
		t.Errorf("Put called %d times for below-threshold body, want 0", store.putCalls)
	}
}

func TestEmitBody_SpillAtThreshold(t *testing.T) {
	store := &fakeStore{}
	body := make([]byte, 1024)
	b := EmitBody(context.Background(), store, 1024, body, "application/json", "id1", "request", false, nil)
	if b.Kind != "spill" {
		t.Errorf("Kind = %q, want spill", b.Kind)
	}
	if store.putCalls != 1 {
		t.Errorf("Put called %d times, want 1", store.putCalls)
	}
	if b.SpillRef == nil || b.SpillRef.Backend != "fake" {
		t.Errorf("SpillRef = %+v, want backend=fake", b.SpillRef)
	}
}

func TestEmitBody_FallsBackToInlineOnPutError(t *testing.T) {
	store := &fakeStore{failPut: true}
	body := make([]byte, 2048)
	b := EmitBody(context.Background(), store, 1024, body, "", "id1", "request", false, nil)
	if b.Kind != "inline" {
		t.Errorf("Kind = %q, want inline (Put failed → fallback)", b.Kind)
	}
}

func TestEmitBody_NoStoreFallsBackToInline(t *testing.T) {
	body := make([]byte, 2048)
	b := EmitBody(context.Background(), nil, 1024, body, "", "id1", "request", false, nil)
	if b.Kind != "inline" {
		t.Errorf("Kind = %q, want inline (no store → inline regardless of size)", b.Kind)
	}
}

func TestEmitBody_PutErrorWithLoggerEmitsWarning(t *testing.T) {
	// Same fallback as TestEmitBody_FallsBackToInlineOnPutError but with a
	// non-nil logger to pin the warn-emit branch. Operators rely on this
	// log line to spot misconfigured spill backends in prod.
	store := &fakeStore{failPut: true}
	body := make([]byte, 2048)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	b := EmitBody(context.Background(), store, 1024, body, "application/json", "evt-1", "request", false, logger)
	if b.Kind != "inline" {
		t.Errorf("Kind = %q, want inline", b.Kind)
	}
	if !strings.Contains(buf.String(), "spillstore Put failed") {
		t.Errorf("expected warn log not emitted: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "backend=fake") {
		t.Errorf("warn log should carry backend attr: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "eventId=evt-1") {
		t.Errorf("warn log should carry eventId attr: %q", buf.String())
	}
}
