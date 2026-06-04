package assistant

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/labstack/echo/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
)

// streamhelper_test.go adapts the P2b command/data-stream split for tests. A turn is
// now STARTED by POST .../sessions/:id/chat (202, runs detached) and OBSERVED over GET
// .../sessions/:id/stream. driveTurn drives both and returns the collected SSE body so
// the existing event-shape assertions (text / tool / usage / error / done) keep working
// against the split protocol.

var driveTurnSeq int32

// driveTurn starts a turn for a fresh session and streams it to completion, returning
// the StartChat status and the SSE body. userID defaults to "user-1" (a real bearer
// always resolves a principal; the bus + isolation key require one). bodyJSON is the
// POST body ({"message":...,"model":...}); the session id is a generated path param.
func driveTurn(t *testing.T, h *Handler, userID, bodyJSON string) (startCode int, sseBody string) {
	t.Helper()
	sid := fmt.Sprintf("sess-%d", atomic.AddInt32(&driveTurnSeq, 1))
	return driveTurnSID(t, h, userID, sid, bodyJSON)
}

// driveTurnSID is driveTurn with an explicit session id (for multi-turn continuation).
func driveTurnSID(t *testing.T, h *Handler, userID, sid, bodyJSON string) (startCode int, sseBody string) {
	t.Helper()
	if userID == "" {
		userID = "user-1"
	}
	e := echo.New()

	// POST .../sessions/:id/chat — starts the turn (202) in the background.
	startReq := httptest.NewRequest(http.MethodPost, "/api/admin/assistant/sessions/"+sid+"/chat", strings.NewReader(bodyJSON))
	startReq.Header.Set("Content-Type", "application/json")
	startReq.Header.Set("Authorization", "Bearer test-jwt")
	startRec := httptest.NewRecorder()
	sc := e.NewContext(startReq, startRec)
	sc.SetParamNames("id")
	sc.SetParamValues(sid)
	sc.Set("adminAuth", &auth.AdminAuth{KeyID: userID})
	if err := h.StartChat(sc); err != nil {
		t.Fatalf("StartChat: %v", err)
	}
	if startRec.Code != http.StatusAccepted {
		return startRec.Code, startRec.Body.String()
	}

	// GET .../sessions/:id/stream — attaches, replays from seq 0, streams live until the
	// turn finishes (the bus closes the channel). Blocks until done, like the old POST.
	streamReq := httptest.NewRequest(http.MethodGet, "/api/admin/assistant/sessions/"+sid+"/stream", nil)
	streamReq.Header.Set("Authorization", "Bearer test-jwt")
	streamRec := httptest.NewRecorder()
	gc := e.NewContext(streamReq, streamRec)
	gc.SetParamNames("id")
	gc.SetParamValues(sid)
	gc.Set("adminAuth", &auth.AdminAuth{KeyID: userID})
	if err := h.StreamSession(gc); err != nil {
		t.Fatalf("StreamSession: %v", err)
	}
	return http.StatusAccepted, streamRec.Body.String()
}
