package assistant

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
)

// ctxFor builds an echo context for one of the split endpoints with the :id path param
// and (optionally) a bearer + resolved admin principal.
func ctxFor(method, path, userID, sid, body string) (*httptest.ResponseRecorder, echo.Context) {
	e := echo.New()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if userID != "" {
		r.Header.Set("Authorization", "Bearer t")
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(r, rec)
	c.SetParamNames("id")
	c.SetParamValues(sid)
	if userID != "" {
		c.Set("adminAuth", &auth.AdminAuth{KeyID: userID})
	}
	return rec, c
}

// TestStartChat_SerializesConcurrentTurns is the server-side half of the "no new
// command while a turn is in flight" rule: a second chat for a session with a running
// turn is 409 (the client also disables input; this is defense-in-depth).
func TestStartChat_SerializesConcurrentTurns(t *testing.T) {
	h := New(Config{SystemVK: "nvk_test", Model: "m"})
	// Occupy the turn slot directly (stands in for an in-flight turn).
	h.bus.startTurn("u-1:s1", context.Background(), time.Minute)

	rec, c := ctxFor(http.MethodPost, "/api/admin/assistant/sessions/s1/chat", "u-1", "s1", `{"message":"hi"}`)
	if err := h.StartChat(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("a second chat on a busy session must be 409, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "turn_in_progress") {
		t.Fatalf("409 body must name turn_in_progress, got %s", rec.Body.String())
	}
}

// TestStartChat_RejectsInvalidSessionID covers the input-hygiene guard on the path id.
func TestStartChat_RejectsInvalidSessionID(t *testing.T) {
	h := New(Config{SystemVK: "nvk_test", Model: "m"})
	rec, c := ctxFor(http.MethodPost, "/api/admin/assistant/sessions/x/chat", "u-1", "bad id!", `{"message":"hi"}`)
	if err := h.StartChat(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an invalid session id must be 400, got %d", rec.Code)
	}
}

// TestInterruptSession covers both outcomes: 204 when a running turn is stopped, 409
// when nothing is in flight.
func TestInterruptSession(t *testing.T) {
	h := New(Config{SystemVK: "nvk_test", Model: "m"})

	rec, c := ctxFor(http.MethodPost, "/api/admin/assistant/sessions/s1/interrupt", "u-1", "s1", "")
	if err := h.InterruptSession(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("interrupt with no in-flight turn must be 409, got %d", rec.Code)
	}

	h.bus.startTurn("u-1:s1", context.Background(), time.Minute)
	rec2, c2 := ctxFor(http.MethodPost, "/api/admin/assistant/sessions/s1/interrupt", "u-1", "s1", "")
	if err := h.InterruptSession(c2); err != nil {
		t.Fatal(err)
	}
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("interrupt of a running turn must be 204, got %d", rec2.Code)
	}
}

// TestInterruptSession_RejectsNonBearer pins the bearer gate on the stop endpoint.
func TestInterruptSession_RejectsNonBearer(t *testing.T) {
	h := New(Config{SystemVK: "nvk_test"})
	rec, c := ctxFor(http.MethodPost, "/api/admin/assistant/sessions/s1/interrupt", "", "s1", "")
	if err := h.InterruptSession(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a non-bearer interrupt must be 422, got %d", rec.Code)
	}
}

// TestStreamSession_NotFound: a GET stream for a session that never started is 404 so
// the client knows to (re)POST a chat rather than wait forever.
func TestStreamSession_NotFound(t *testing.T) {
	h := New(Config{SystemVK: "nvk_test"})
	rec, c := ctxFor(http.MethodGet, "/api/admin/assistant/sessions/nope/stream", "u-1", "nope", "")
	if err := h.StreamSession(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("streaming an unknown session must be 404, got %d", rec.Code)
	}
}

// TestStreamSession_RejectsNonBearer + invalid id guards on the stream endpoint.
func TestStreamSession_Guards(t *testing.T) {
	h := New(Config{SystemVK: "nvk_test"})
	rec, c := ctxFor(http.MethodGet, "/api/admin/assistant/sessions/s1/stream", "", "s1", "")
	if err := h.StreamSession(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a non-bearer stream must be 422, got %d", rec.Code)
	}

	rec2, c2 := ctxFor(http.MethodGet, "/api/admin/assistant/sessions/x/stream", "u-1", "bad!", "")
	if err := h.StreamSession(c2); err != nil {
		t.Fatal(err)
	}
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("an invalid session id on stream must be 400, got %d", rec2.Code)
	}
}

// TestStreamSession_ReplaysFinishedTurn drives the replay path: a turn that already
// published its events (here directly on the bus) is streamed back in full from seq 0.
func TestStreamSession_ReplaysFinishedTurn(t *testing.T) {
	h := New(Config{SystemVK: "nvk_test"})
	h.bus.startTurn("u-1:s1", context.Background(), time.Minute)
	h.bus.publish("u-1:s1", "text", map[string]string{"delta": "hello"})
	h.bus.publish("u-1:s1", "done", map[string]any{"sessionId": "s1"})
	h.bus.finishTurn("u-1:s1")

	rec, c := ctxFor(http.MethodGet, "/api/admin/assistant/sessions/s1/stream", "u-1", "s1", "")
	if err := h.StreamSession(c); err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "event: text") || !strings.Contains(out, "hello") || !strings.Contains(out, "event: done") {
		t.Fatalf("a reconnect must replay the finished turn's events, got:\n%s", out)
	}
}

// TestStreamSession_ReconnectFromLastSeq replays only events newer than lastSeq.
func TestStreamSession_ReconnectFromLastSeq(t *testing.T) {
	h := New(Config{SystemVK: "nvk_test"})
	h.bus.startTurn("u-1:s1", context.Background(), time.Minute)
	h.bus.publish("u-1:s1", "text", map[string]string{"delta": "one"})
	h.bus.publish("u-1:s1", "text", map[string]string{"delta": "two"})
	h.bus.finishTurn("u-1:s1")

	rec, c := ctxFor(http.MethodGet, "/api/admin/assistant/sessions/s1/stream?lastSeq=1", "u-1", "s1", "")
	if err := h.StreamSession(c); err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	if strings.Contains(out, "one") {
		t.Fatalf("lastSeq=1 must not replay the first event, got:\n%s", out)
	}
	if !strings.Contains(out, "two") {
		t.Fatalf("lastSeq=1 must replay the second event, got:\n%s", out)
	}
}

// TestStartChat_RejectsBearerWithoutPrincipal: a bearer that does not resolve to an
// admin principal (empty KeyID) cannot self-call or key the bus — 422, not a turn.
func TestStartChat_RejectsBearerWithoutPrincipal(t *testing.T) {
	h := New(Config{SystemVK: "nvk_test", Model: "m"})
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/assistant/sessions/s1/chat", strings.NewReader(`{"message":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer t") // bearer present, but no adminAuth in context
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("s1")
	if err := h.StartChat(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a bearer with no resolved principal must be 422, got %d", rec.Code)
	}
}

// TestValidSessionID_Bounds covers the length + charset rejections directly.
func TestValidSessionID_Bounds(t *testing.T) {
	if validSessionID("") || validSessionID(strings.Repeat("a", 129)) || validSessionID("has space") {
		t.Fatal("empty, over-long, and unsafe-charset ids must be rejected")
	}
	if !validSessionID("abc-123_DEF.x:y") {
		t.Fatal("a normal id (hex / uuid / colon-scoped) must be accepted")
	}
}

// TestStreamSession_GapSignalled: a reconnect older than the ring window emits a gap
// notice so the client knows history was truncated.
func TestStreamSession_GapSignalled(t *testing.T) {
	h := New(Config{SystemVK: "nvk_test"})
	h.bus.startTurn("u-1:s1", context.Background(), time.Minute)
	for range replayRingSize + 10 {
		h.bus.publish("u-1:s1", "text", map[string]string{"delta": "x"})
	}
	h.bus.finishTurn("u-1:s1")

	rec, c := ctxFor(http.MethodGet, "/api/admin/assistant/sessions/s1/stream", "u-1", "s1", "")
	if err := h.StreamSession(c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Body.String(), "event: gap") {
		t.Fatalf("a reconnect past the ring window must emit a gap event")
	}
}

// TestStreamSession_ClientDisconnectArmsGrace drives the billing-critical disconnect
// path through the real handler: a client that opens the stream and then drops (request
// ctx cancelled) must arm the grace that cancels the abandoned, still-running turn.
func TestStreamSession_ClientDisconnectArmsGrace(t *testing.T) {
	orig := streamGrace
	streamGrace = 40 * time.Millisecond
	defer func() { streamGrace = orig }()

	h := New(Config{SystemVK: "nvk_test"})
	_, turnCtx, _ := h.bus.startTurn("u-1:s1", context.Background(), time.Minute)

	e := echo.New()
	cctx, ccancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/admin/assistant/sessions/s1/stream", nil).WithContext(cctx)
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("s1")
	c.Set("adminAuth", &auth.AdminAuth{KeyID: "u-1"})

	streamDone := make(chan struct{})
	go func() {
		_ = h.StreamSession(c)
		close(streamDone)
	}()
	time.Sleep(15 * time.Millisecond) // let StreamSession attach (cancels the start-grace) and block
	ccancel()                         // the client disconnects

	select {
	case <-streamDone:
	case <-time.After(time.Second):
		t.Fatal("StreamSession must return when the client disconnects")
	}
	select {
	case <-turnCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("a client disconnect must arm the grace that cancels the abandoned turn")
	}
}

// TestInterrupt_EmitsTurnAborted is the AC-4 abort path end-to-end: a running turn
// interrupted mid-inference surfaces turn_aborted (not an error) on the stream.
func TestInterrupt_EmitsTurnAborted(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/chat/completions") {
			select { // block until the turn ctx is cancelled by the interrupt
			case <-r.Context().Done():
			case <-release:
			case <-time.After(2 * time.Second):
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
	}))
	defer mock.Close()

	h := New(Config{AIGatewayURL: mock.URL, CPBaseURL: mock.URL, SystemVK: "nvk_test", Model: "m"})
	rec, c := ctxFor(http.MethodPost, "/api/admin/assistant/sessions/s1/chat", "u-1", "s1", `{"message":"hi"}`)
	if err := h.StartChat(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("StartChat = %d, want 202", rec.Code)
	}

	// The turn slot is claimed synchronously in StartChat, so interrupt finds it.
	irec, ic := ctxFor(http.MethodPost, "/api/admin/assistant/sessions/s1/interrupt", "u-1", "s1", "")
	if err := h.InterruptSession(ic); err != nil {
		t.Fatal(err)
	}
	if irec.Code != http.StatusNoContent {
		t.Fatalf("interrupt = %d, want 204", irec.Code)
	}

	// Stream the (now finishing) turn; it must carry turn_aborted.
	grec, gc := ctxFor(http.MethodGet, "/api/admin/assistant/sessions/s1/stream", "u-1", "s1", "")
	if err := h.StreamSession(gc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(grec.Body.String(), "turn_aborted") {
		t.Fatalf("an interrupted turn must emit turn_aborted, got:\n%s", grec.Body.String())
	}
}
