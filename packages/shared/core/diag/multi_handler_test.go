package diag

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// captureHandler is a slog.Handler that records every Handle call's level +
// message into a slice.
type captureHandler struct {
	level slog.Level
	got   []string
}

func (c *captureHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= c.level }

func (c *captureHandler) Handle(_ context.Context, r slog.Record) error {
	c.got = append(c.got, r.Level.String()+":"+r.Message)
	return nil
}
func (c *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return c }
func (c *captureHandler) WithGroup(_ string) slog.Handler      { return c }

func TestMultiHandlerFansOutToAllEnabled(t *testing.T) {
	infoCh := &captureHandler{level: slog.LevelInfo}
	errCh := &captureHandler{level: slog.LevelError}

	var buf bytes.Buffer
	jsonH := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})

	m := NewMultiHandler(infoCh, errCh, jsonH, nil)
	logger := slog.New(m)
	logger.Info("hello")
	logger.Error("oops")

	// info handler sees both
	if got := infoCh.got; len(got) != 2 || got[0] != "INFO:hello" || got[1] != "ERROR:oops" {
		t.Errorf("info handler got = %v", got)
	}
	// error handler sees only error
	if got := errCh.got; len(got) != 1 || got[0] != "ERROR:oops" {
		t.Errorf("error handler got = %v", got)
	}
	// stdout JSON handler also wrote both
	if !strings.Contains(buf.String(), `"msg":"hello"`) || !strings.Contains(buf.String(), `"msg":"oops"`) {
		t.Errorf("json handler missed messages: %s", buf.String())
	}
}

func TestMultiHandlerEnabledIsAny(t *testing.T) {
	infoCh := &captureHandler{level: slog.LevelInfo}
	warnCh := &captureHandler{level: slog.LevelWarn}
	m := NewMultiHandler(infoCh, warnCh)

	if !m.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Enabled(Info) = false; want true (info handler accepts)")
	}
	if !m.Enabled(context.Background(), slog.LevelError) {
		t.Error("Enabled(Error) = false; want true")
	}
	if m.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("Enabled(Debug) = true; want false (no handler accepts)")
	}
}

// errCaptureHandler returns a sentinel error from Handle to exercise the
// MultiHandler error-aggregation path.
type errCaptureHandler struct {
	level slog.Level
	err   error
}

func (e *errCaptureHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= e.level }
func (e *errCaptureHandler) Handle(context.Context, slog.Record) error    { return e.err }
func (e *errCaptureHandler) WithAttrs([]slog.Attr) slog.Handler           { return e }
func (e *errCaptureHandler) WithGroup(string) slog.Handler                { return e }

func TestMultiHandler_HandleReturnsFirstError(t *testing.T) {
	// MultiHandler must return the FIRST handler's error, not the last,
	// and must keep calling subsequent handlers even after one errors —
	// otherwise a downstream stderr sink could mask a Hub-push failure
	// or vice versa.
	hErr := &errCaptureHandler{level: slog.LevelInfo, err: errors.New("boom")}
	hOK := &captureHandler{level: slog.LevelInfo}
	m := NewMultiHandler(hErr, hOK)

	logger := slog.New(m)
	logger.Error("msg")

	if len(hOK.got) != 1 {
		t.Errorf("second handler should still be called even when first errored: got=%v", hOK.got)
	}

	// Direct Handle call to assert error returned.
	rec := slog.NewRecord(time.Now(), slog.LevelError, "x", 0)
	if err := m.Handle(context.Background(), rec); err == nil {
		t.Error("Handle should propagate first error")
	}
}

// attrCaptureHandler records WithAttrs / WithGroup calls so the fan-out
// is observable.
type attrCaptureHandler struct {
	attrs   []slog.Attr
	groups  []string
	wrapped int
}

func (a *attrCaptureHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (a *attrCaptureHandler) Handle(context.Context, slog.Record) error { return nil }
func (a *attrCaptureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &attrCaptureHandler{
		attrs:   append(append([]slog.Attr{}, a.attrs...), attrs...),
		groups:  append([]string{}, a.groups...),
		wrapped: a.wrapped + 1,
	}
}
func (a *attrCaptureHandler) WithGroup(name string) slog.Handler {
	return &attrCaptureHandler{
		attrs:   append([]slog.Attr{}, a.attrs...),
		groups:  append(append([]string{}, a.groups...), name),
		wrapped: a.wrapped + 1,
	}
}

func TestMultiHandlerWithAttrsPropagatesToEveryHandler(t *testing.T) {
	// WithAttrs must call WithAttrs on EACH underlying handler — without
	// the fan-out, slog.With on the multi-handler would silently drop
	// attrs on all but one downstream.
	h1 := &attrCaptureHandler{}
	h2 := &attrCaptureHandler{}
	m := NewMultiHandler(h1, h2)

	wrapped := m.WithAttrs([]slog.Attr{slog.String("k", "v")})
	wm, ok := wrapped.(*MultiHandler)
	if !ok {
		t.Fatalf("WithAttrs returned %T, want *MultiHandler", wrapped)
	}
	if len(wm.handlers) != 2 {
		t.Errorf("handler count: %d, want 2", len(wm.handlers))
	}
	for i, h := range wm.handlers {
		ac, ok := h.(*attrCaptureHandler)
		if !ok {
			t.Fatalf("handler[%d]: %T", i, h)
		}
		if len(ac.attrs) != 1 || ac.attrs[0].Key != "k" {
			t.Errorf("handler[%d] attrs: %+v", i, ac.attrs)
		}
	}
}

func TestMultiHandlerWithGroupPropagatesToEveryHandler(t *testing.T) {
	h1 := &attrCaptureHandler{}
	h2 := &attrCaptureHandler{}
	m := NewMultiHandler(h1, h2)

	wrapped := m.WithGroup("grp1")
	wm, ok := wrapped.(*MultiHandler)
	if !ok {
		t.Fatalf("WithGroup returned %T", wrapped)
	}
	for i, h := range wm.handlers {
		ac, ok := h.(*attrCaptureHandler)
		if !ok {
			t.Fatalf("handler[%d]: %T", i, h)
		}
		if len(ac.groups) != 1 || ac.groups[0] != "grp1" {
			t.Errorf("handler[%d] groups: %+v", i, ac.groups)
		}
	}
}
