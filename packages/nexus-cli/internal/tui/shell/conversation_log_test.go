package shell

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
)

// logCapture is a minimal slog.Handler recording each record's message + the
// "err" attribute so a test can assert a user-visible turn failure was mirrored
// into the diagnostic log.
type logCapture struct {
	mu   sync.Mutex
	msgs []string
	errs []string
}

func (h *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (h *logCapture) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *logCapture) WithGroup(string) slog.Handler            { return h }
func (h *logCapture) Handle(_ context.Context, r slog.Record) error {
	var errVal string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "err" {
			errVal = a.Value.String()
		}
		return true
	})
	h.mu.Lock()
	h.msgs = append(h.msgs, r.Message)
	h.errs = append(h.errs, errVal)
	h.mu.Unlock()
	return nil
}

// TestFinish_MirrorsTurnErrorToLog asserts that when a turn ends in error the
// conversation both shows the "⚠ … — ask again" sys line AND records it in the
// diagnostic log with the error string (the seam an operator's "it just errors"
// report is matched against).
func TestFinish_MirrorsTurnErrorToLog(t *testing.T) {
	cap := &logCapture{}
	c := newConversation(testSession(), nil)
	c.log = slog.New(cap)

	turnErr := errors.New("context deadline exceeded")
	c.finish(agentDoneMsg{err: turnErr})

	if !containsLineTag(c.lines, "sys") {
		t.Fatal("a turn error must surface a sys line to the user")
	}
	if len(cap.msgs) != 1 || cap.msgs[0] != "turn failed" {
		t.Fatalf("log messages = %v, want one 'turn failed'", cap.msgs)
	}
	if cap.errs[0] != turnErr.Error() {
		t.Errorf("logged err = %q, want %q", cap.errs[0], turnErr.Error())
	}
}

// TestSubmit_BuildFailureMirroredToLog asserts an agent-build failure on submit
// is logged (and shown) — the other user-visible failure seam.
func TestSubmit_BuildFailureMirroredToLog(t *testing.T) {
	cap := &logCapture{}
	c := newConversation(testSession(), nil) // nil build → ensureAgent fails
	c.log = slog.New(cap)

	c.submit("do a thing")

	if len(cap.msgs) != 1 || cap.msgs[0] != "agent build failed" {
		t.Fatalf("log messages = %v, want one 'agent build failed'", cap.msgs)
	}
	if cap.errs[0] == "" {
		t.Error("agent-build failure logged with empty err")
	}
}

// TestLogTurnErr_NilLogAndNilErr asserts the helper no-ops when either the logger
// or the error is nil (so an unconfigured shell never panics).
func TestLogTurnErr_NilLogAndNilErr(t *testing.T) {
	c := newConversation(testSession(), nil)

	// nil logger: must not panic.
	c.log = nil
	c.logTurnErr("x", errors.New("boom"))

	// nil error with a real logger: nothing recorded.
	cap := &logCapture{}
	c.log = slog.New(cap)
	c.logTurnErr("x", nil)
	if len(cap.msgs) != 0 {
		t.Errorf("logTurnErr(nil err) recorded %v, want nothing", cap.msgs)
	}
}
