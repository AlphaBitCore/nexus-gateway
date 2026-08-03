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

// TestEmitBody_FallsBackToInlineOnPutError previously asserted only that the Kind was inline,
// which passed whether the fallback kept 2 KiB or 200 MiB. Finding S-1 is precisely about the
// SIZE, so the assertions below pin the bound and the honesty of the record.
func TestEmitBody_FallsBackToInlineOnPutError(t *testing.T) {
	store := &fakeStore{failPut: true}
	const threshold = 1024
	body := make([]byte, 2048)
	b := EmitBody(context.Background(), store, threshold, body, "", "id1", "request", false, nil)

	if b.Kind != "inline" {
		t.Fatalf("Kind = %q, want inline (Put failed → fallback)", b.Kind)
	}
	// The bound. Without it a body large enough to spill is inlined whole into the audit row
	// and the MQ message — a 200 MiB payload becomes a 200 MiB NATS publish, which max_payload
	// refuses, so "don't drop the row" would lose the row anyway after spending the memory
	// three times over.
	if got := int64(len(b.InlineBytes)); got > threshold {
		t.Errorf("kept %d inline bytes against a %d threshold. A failed spill must keep at most "+
			"what an inline body is already allowed to carry.", got, threshold)
	}
	// The honesty. A prefix presented as a whole body is silent corruption of the audit record.
	if !b.Truncated {
		t.Error("Truncated is false after a bounded fallback: the record claims to hold the whole " +
			"body while carrying a prefix")
	}
	// The real size must survive, so a reader can see how much is missing.
	if b.SizeBytes != int64(len(body)) {
		t.Errorf("SizeBytes = %d, want %d — the record must report the body's REAL size even when "+
			"it only carries a prefix", b.SizeBytes, len(body))
	}
}

// TestEmitBody_PutErrorBelowThresholdKeepsEverything is the other side of the bound: a body that
// happens to be under the threshold is not truncated and is not falsely marked as a prefix. Only
// the over-threshold case is a prefix.
func TestEmitBody_PutErrorBelowThresholdKeepsEverything(t *testing.T) {
	store := &fakeStore{failPut: true}
	body := []byte("small body that only spilled because the caller asked it to")
	b := EmitBody(context.Background(), store, int64(len(body)), body, "", "id1", "request", false, nil)

	if string(b.InlineBytes) != string(body) {
		t.Errorf("InlineBytes = %q, want the whole body — it fits under the threshold", b.InlineBytes)
	}
	if b.Truncated {
		t.Error("Truncated is true for a body that was kept in full; a false truncation marker " +
			"makes a complete record look incomplete")
	}
}

// With no spill backend an oversize body is inlined — but BOUNDED (finding S-6).
//
// This test previously asserted only Kind == "inline", which is true whether the
// fallback keeps 2 KiB or 200 MiB, so it could not have caught the defect it sits
// on: every *.config.yaml ships spill disabled, so this is the COMMON production
// path, and it was storing whole bodies inline under a setting named
// MaxInlineBodyBytes. Exactly the weakness S-1's test had on the failed-Put arm.
func TestEmitBody_NoStoreBoundsAnOversizeBody(t *testing.T) {
	const threshold = 1024
	body := make([]byte, 2048)
	for i := range body {
		body[i] = byte(i % 251)
	}
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	b := EmitBody(context.Background(), nil, threshold, body, "application/json", "id1", "request", false, logger)

	if b.Kind != "inline" {
		t.Fatalf("Kind = %q, want inline: no backend must still produce a row", b.Kind)
	}
	if int64(len(b.InlineBytes)) > threshold {
		t.Errorf("kept %d inline bytes against a %d threshold — the no-backend path is unbounded, "+
			"which is what puts a whole oversize body into the audit row and the MQ message",
			len(b.InlineBytes), threshold)
	}
	if !b.Truncated {
		t.Error("Truncated is false after a bounded no-backend inline: the record claims to hold " +
			"the whole body while carrying a prefix")
	}
	if b.SizeBytes != int64(len(body)) {
		t.Errorf("SizeBytes = %d, want %d — the REAL size must survive so a reader can see how much "+
			"is missing", b.SizeBytes, len(body))
	}
	if !bytes.Equal(b.InlineBytes, body[:threshold]) {
		t.Error("the kept bytes are not the body's leading prefix")
	}
	if !strings.Contains(buf.String(), "no spill backend is configured") {
		t.Errorf("no operator-facing warning was logged; truncating an audit body silently is the "+
			"failure this whole program exists to remove. got: %s", buf.String())
	}
}

// The bound must not touch a body that is already inside it: this is the ordinary
// case and truncating it would be a data-loss regression dressed as a fix.
func TestEmitBody_NoStoreKeepsAnUndersizeBodyWhole(t *testing.T) {
	const threshold = 1024
	body := make([]byte, 512)
	b := EmitBody(context.Background(), nil, threshold, body, "", "id1", "request", false, nil)
	if len(b.InlineBytes) != len(body) {
		t.Errorf("kept %d of %d bytes: a body below the threshold must be stored whole",
			len(b.InlineBytes), len(body))
	}
	if b.Truncated {
		t.Error("Truncated is true for a body that was stored whole")
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
