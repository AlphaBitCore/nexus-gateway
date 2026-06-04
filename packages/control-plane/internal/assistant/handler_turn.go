package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	cpmetrics "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/labstack/echo/v4"
)

// recordTurn increments the per-turn outcome counter (spec §7 / NFR-13). Nil-safe:
// the instrument is unbound until cpmetrics.Register runs at startup, so pool-less
// tests and the early-boot window record nothing rather than panic.
func recordTurn(result string) {
	if cpmetrics.AssistantTurnsTotal != nil {
		cpmetrics.AssistantTurnsTotal.With(result).Inc()
	}
}

// StartChat starts one agent turn for the session and returns 202 immediately; the
// turn runs DETACHED in a background goroutine and its work is observed over GET
// .../stream. A second chat while a turn is already in flight for the same session is
// rejected 409 (turn_in_progress) — the server-side half of the "no new command while
// one is running" rule (the client also disables input). The session id is a client-
// supplied path param (a fresh id starts a new conversation; an owned id continues it).
func (h *Handler) StartChat(c echo.Context) error {
	id := c.Param("id")
	if !validSessionID(id) {
		return errJSON(c, http.StatusBadRequest, "validation_error", "a valid session id is required")
	}
	var body struct {
		Message string `json:"message"`
		Model   string `json:"model"` // optional: client-chosen model (validated against the allow-list)
	}
	if err := c.Bind(&body); err != nil || strings.TrimSpace(body.Message) == "" {
		return errJSON(c, http.StatusBadRequest, "validation_error", "message is required")
	}
	if h.cfg.SystemVK == "" {
		recordTurn("unavailable")
		return errJSON(c, http.StatusServiceUnavailable, "unavailable", "assistant inference is not configured")
	}
	authorization, userID, ok := h.callerBearer(c)
	if !ok {
		recordTurn("unsupported_auth")
		return nil // callerBearer wrote the 422
	}

	key := userID + ":" + id
	// Claim a turn slot. started=false → a turn is already in flight for this session
	// (serialization guard). The turn ctx is detached (background) + carries the
	// wall-clock deadline, so it OUTLIVES this POST: the SSE stream can reconnect.
	_, turnCtx, started := h.bus.startTurn(key, context.Background(), h.turnDeadline)
	if !started {
		return errJSON(c, http.StatusConflict, "turn_in_progress",
			"a turn is already running for this session; wait for it to finish or stop it first")
	}

	// Claim this instance as the session owner (multi-replica 421 safety net). Detached
	// short ctx — independent of the turn ctx — so an interrupt/deadline cannot abort
	// the ownership write while a confirm is about to be parked.
	claimCtx, claimCancel := context.WithTimeout(context.Background(), 3*time.Second)
	h.owners.claim(claimCtx, key)
	claimCancel()

	// Capture request-scoped values BEFORE returning — c is not safe to touch once the
	// handler returns and the turn goroutine runs on.
	turn := turnParams{
		key:           key,
		userID:        userID,
		sessionID:     id,
		message:       body.Message,
		model:         h.resolveModel(body.Model),
		authorization: authorization,
		sourceIP:      c.RealIP(),
		requestID:     middleware.NexusRequestIDFromContext(c),
	}
	go h.runTurn(turnCtx, turn)

	return c.JSON(http.StatusAccepted, map[string]any{"sessionId": id, "seq": 0})
}

// turnParams carries the request-scoped inputs a background turn needs (read off the
// echo context in StartChat, since the context is not safe to use after the handler
// returns).
type turnParams struct {
	key           string
	userID        string
	sessionID     string
	message       string
	model         string
	authorization string
	sourceIP      string
	requestID     string
}

// runTurn executes one agent turn in the background, publishing its work to the
// session bus (consumed by GET .../stream). It always finishes the turn on the bus —
// finishTurn flips the running flag, wakes the stream to drain + close, and clears the
// cancel — so a panic or early return cannot wedge the session.
func (h *Handler) runTurn(ctx context.Context, p turnParams) {
	defer h.bus.finishTurn(p.key)
	pub := func(event string, payload any) { h.bus.publish(p.key, event, payload) }

	session := agent.NewSession("web")
	session.ID = p.sessionID

	// Per-caller stores: DB-backed (isolated by the authenticated userId) when a pool
	// is wired, else in-memory (tests / pool-less dev). userId is the authenticated
	// principal, never client input (I3).
	var store agent.SessionStore = newMemStore()
	var mem agent.MemoryStore = newMemMemory()
	if h.cfg.Pool != nil && p.userID != "" {
		mem = newDBMemory(ctx, h.cfg.Pool, p.userID)
		if h.cfg.Spill != nil { // transcripts persist only when object storage is also wired
			store = newDBStore(ctx, h.cfg.Pool, h.cfg.Spill, p.userID)
		}
	}
	// Continue an owned conversation; a missing / non-owned id silently starts fresh
	// (Load is userId-scoped, so a cross-user id never resolves).
	if loaded, lerr := store.Load(p.sessionID); lerr == nil {
		session = loaded
	}

	var files fileStore
	if h.cfg.Pool != nil && h.cfg.Spill != nil && p.userID != "" {
		files = newWebFileStore(ctx, h.cfg.Pool, h.cfg.Spill, p.userID, p.sessionID)
	}

	// Populated right after BuildWebAgent; the OnToolEnd closure reads it during the
	// turn (by then set; an empty map degrades safely to tool="unknown").
	knownTools := map[string]struct{}{}
	ag, err := BuildWebAgent(WebAgentDeps{
		CallerAuthorization: p.authorization,
		CallerSourceIP:      p.sourceIP, // R3: stamp the human actor's IP on in-process self-calls
		CallerRequestID:     p.requestID,
		Dispatcher:          h.cfg.Dispatcher,
		CPBaseURL:           h.cfg.CPBaseURL,
		AIGatewayURL:        h.cfg.AIGatewayURL,
		SystemVK:            h.cfg.SystemVK,
		Model:               p.model,
		IsProd:              h.cfg.IsProd,
		Memory:              mem,
		Store:               store,
		Session:             session,
		Files:               files,
		SituationCache:      h.situations, // NFR-11: per-caller situation memoization
		SituationKey:        p.userID,
		OnText:              func(s string) { pub("text", map[string]string{"delta": s}) },
		OnReasoning:         func(s string) { pub("reasoning", map[string]string{"delta": s}) },
		OnToolStart: func(name string, input []byte) {
			pub("tool_start", map[string]any{"name": name, "input": json.RawMessage(input)})
		},
		OnToolEnd: func(name string, output []byte, isErr bool) {
			pub("tool_end", map[string]any{"name": name, "isError": isErr})
			// Structured download signal: when write_file succeeds, surface the file as
			// its own `file` SSE event sourced from the tool's own output, so the UI's
			// download button no longer depends on the model echoing the URL into prose.
			if name == "write_file" && !isErr {
				if id, ok := fileIDFromToolOutput(string(output)); ok {
					pub("file", map[string]string{"id": id, "downloadPath": assistantFilesPath + id})
				}
			}
			if cpmetrics.AssistantToolInvocationsTotal != nil {
				result := "ok"
				if isErr {
					result = "error"
				}
				// `name` is the MODEL-emitted tool name, not a trusted constant: clamp
				// it to the agent's actual tool set so an arbitrary string can never
				// become an unbounded metric label.
				tool := "unknown"
				if _, ok := knownTools[name]; ok {
					tool = name
				}
				cpmetrics.AssistantToolInvocationsTotal.With(tool, result).Inc()
			}
		},
		OnUsage: func(cs agent.ContextStats) { pub("usage", cs) },
		OnNavigate: func(d NavigateDirective) {
			pub("navigate", d)
			if cpmetrics.AssistantNavigationsTotal != nil {
				cpmetrics.AssistantNavigationsTotal.With().Inc()
			}
		},
		Confirm:          h.makeConfirm(p.userID, p.sessionID, pub),
		Redactor:         h.redactor, // §8: scrub PII from tool output before prompt entry
		DisableBodyReads: h.cfg.DisableBodyReads,
	})
	if err != nil {
		pub("error", map[string]string{"code": "agent_build_failed", "message": "could not start the assistant"})
		recordTurn("error")
		pub("done", map[string]any{"sessionId": p.sessionID, "seq": 0})
		return
	}
	for _, n := range ag.ToolNames() {
		knownTools[n] = struct{}{}
	}

	turnResult := "ok"
	if _, turnErr := ag.Turn(ctx, p.message, ""); turnErr != nil {
		turnResult = "error"
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			// Fixed user-facing message — never leak the internal gateway URL or
			// transport details turnErr.Error() carries on a hung upstream.
			pub("error", map[string]string{"code": "turn_deadline", "message": "the assistant took too long and was stopped"})
		case errors.Is(ctx.Err(), context.Canceled):
			// Interrupt (Stop) or the disconnect-grace cancel — user-initiated, not a
			// failure. AC-4: an aborted turn emits turn_aborted (no error bubble).
			turnResult = "aborted"
			pub("turn_aborted", map[string]any{"sessionId": p.sessionID})
		default:
			// Fixed user-facing message — turnErr.Error() can carry the internal AI
			// Gateway URL / transport details on an upstream failure (same leak the
			// turn_deadline branch avoids). The failure is still counted in metrics.
			pub("error", map[string]string{"code": "turn_failed", "message": "the assistant hit an error and stopped"})
		}
	}
	recordTurn(turnResult)
	pub("done", map[string]any{"sessionId": p.sessionID, "seq": 0})
}

// StreamSession is the long-lived SSE channel for a session's turn. It attaches to the
// bus, writes any events the client missed (replay from ?lastSeq=), then streams live
// events until the turn finishes or the client disconnects. A reconnect (network blip)
// re-opens this with the last seq it saw; the turn keeps running in the meantime, and
// a disconnect with no reconnect within the grace window cancels the turn (bus.detach).
func (h *Handler) StreamSession(c echo.Context) error {
	id := c.Param("id")
	if !validSessionID(id) {
		return errJSON(c, http.StatusBadRequest, "validation_error", "a valid session id is required")
	}
	_, userID, ok := h.callerBearer(c)
	if !ok {
		return nil // callerBearer wrote the 422
	}
	key := userID + ":" + id
	lastSeq := 0
	if v := c.QueryParam("lastSeq"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			lastSeq = n
		}
	}

	ch := make(chan busEvent, liveChanBuffer)
	res, found := h.bus.attach(key, lastSeq, ch)
	if !found {
		// No live session for this key — the turn was never started, or its entry was
		// reclaimed. 404 tells the client to (re)POST a chat rather than wait on a
		// stream that will never produce.
		return errJSON(c, http.StatusNotFound, "not_found", "no active session stream")
	}

	resp := c.Response()
	resp.Header().Set("Content-Type", "text/event-stream")
	resp.Header().Set("Cache-Control", "no-cache")
	resp.Header().Set("Connection", "keep-alive")
	resp.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx)
	resp.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(resp.Writer)
	writeEv := func(ev busEvent) error {
		// Emit the SSE `id:` (the per-session seq) so the client can reconnect with
		// ?lastSeq=<id> and replay only what it missed. The gap notice carries seq 0
		// (never advances the client's cursor).
		if _, err := fmt.Fprintf(resp.Writer, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Event, ev.Data); err != nil {
			return err
		}
		return rc.Flush()
	}

	// A replay gap (the ring rotated past what the client missed) is signalled so the
	// client can note history was truncated rather than silently miss events.
	if res.gap {
		_ = writeEv(busEvent{Event: "gap", Data: json.RawMessage(`{"note":"some earlier events were not retained"}`)})
	}
	for _, ev := range res.replay {
		if writeEv(ev) != nil {
			h.bus.detach(key, ch)
			return nil //nolint:nilerr // a failed SSE write means the client is gone; ending the stream is not a server error
		}
	}
	if res.live == nil {
		// The turn already finished; the replay was the whole transcript. End the stream.
		return nil
	}

	ctx := c.Request().Context()
	for {
		select {
		case ev, alive := <-res.live:
			if !alive {
				return nil // turn finished (bus closed the channel after the last event)
			}
			if writeEv(ev) != nil {
				h.bus.detach(key, ch) // write failed → client gone; start the grace timer
				return nil            //nolint:nilerr // a failed SSE write means the client is gone; ending the stream is not a server error
			}
		case <-ctx.Done():
			h.bus.detach(key, ch) // client disconnected → grace, then cancel if no reconnect
			return nil
		}
	}
}

// InterruptSession cancels the in-flight turn for the session (the Stop button). It is
// idempotent-ish: 204 when a running turn was stopped, 409 when nothing was running
// (already finished / never started) so the client can ignore a late Stop.
func (h *Handler) InterruptSession(c echo.Context) error {
	id := c.Param("id")
	if !validSessionID(id) {
		return errJSON(c, http.StatusBadRequest, "validation_error", "a valid session id is required")
	}
	_, userID, ok := h.callerBearer(c)
	if !ok {
		return nil // callerBearer wrote the 422
	}
	if h.bus.interrupt(userID + ":" + id) {
		return c.NoContent(http.StatusNoContent)
	}
	return errJSON(c, http.StatusConflict, "not_running", "no in-flight turn to stop for this session")
}
