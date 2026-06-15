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
	"unicode/utf8"

	cpmetrics "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/labstack/echo/v4"
)

// recordTurn increments the per-turn outcome counter. Nil-safe:
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
//
// Why the id is client-supplied, not server-generated: the
// command/data-stream split needs the id BEFORE any turn event exists — the
// client must know it to open GET .../stream and to decide fresh-vs-continue —
// so a server-minted id returned only in the 202 body would not fit the flow.
// Crucially this is NOT a cross-session collision/hijack risk: every session is
// resolved within the caller's OWN userId namespace. The bus key is
// `userID + ":" + id` (below), and the persistence + CRUD endpoints are
// userId-scoped (dbStore binds WHERE "userId"; a non-owned id is an
// indistinguishable 404 — see handler_sessions.go). A client therefore can only
// ever "collide" with its own prior session (which is the intended "continue"
// behaviour); it can neither reach nor guess another user's session by picking
// the same id. validSessionID bounds the value to an input-hygiene charset.
func (h *Handler) StartChat(c echo.Context) error {
	id := c.Param("id")
	if !validSessionID(id) {
		return writeErrJSON(c, http.StatusBadRequest, "validation_error", "a valid session id is required")
	}
	var body struct {
		Message string `json:"message"`
		Model   string `json:"model"` // optional: client-chosen model (validated against the allow-list)
	}
	if err := c.Bind(&body); err != nil || strings.TrimSpace(body.Message) == "" {
		return writeErrJSON(c, http.StatusBadRequest, "validation_error", "message is required")
	}
	if h.cfg.SystemVK == "" {
		recordTurn("unavailable")
		return writeErrJSON(c, http.StatusServiceUnavailable, "unavailable", "assistant inference is not configured")
	}
	authorization, userID, ok := h.callerBearer(c)
	if !ok {
		recordTurn("unsupported_auth")
		return nil // callerBearer wrote the 422
	}

	key := userID + ":" + id
	// Claim a turn slot. The turn ctx is detached (background) + carries the wall-clock
	// deadline, so it OUTLIVES this POST: the SSE stream can reconnect. Two distinct
	// refusals: a turn already in flight for THIS session is 409 (serialization guard);
	// the user being at the per-user concurrent-turn cap is 429 (bounds one
	// user's instantaneous share of the shared system VK so they cannot starve others).
	_, turnCtx, sr := h.bus.startTurn(key, context.Background(), h.turnDeadline)
	switch sr {
	case startTurnInFlight:
		return writeErrJSON(c, http.StatusConflict, "turn_in_progress",
			"a turn is already running for this session; wait for it to finish or stop it first")
	case startUserLimitHit:
		recordTurn("user_limit")
		return writeErrJSON(c, http.StatusTooManyRequests, "user_turn_limit_exceeded",
			"you have too many concurrent sessions; finish or stop existing sessions first")
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

	return c.JSON(http.StatusAccepted, map[string]any{"sessionId": id})
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
	// principal, never client input.
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
	ag := BuildWebAgent(WebAgentDeps{
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
		SituationCache:      h.situations, // per-caller situation memoization
		SituationKey:        p.userID,
		OnText:              func(s string) { pub("text", map[string]string{"delta": s}) },
		OnReasoning:         func(s string) { pub("reasoning", map[string]string{"delta": s}) },
		OnToolStart: func(name string, input []byte) {
			pub("tool_start", map[string]any{"name": name, "input": json.RawMessage(input)})
		},
		OnToolEnd: func(name string, output []byte, isErr bool) {
			// The result rides along (capped) so the widget's tool chip can show
			// the response, not just the request. No new exposure: the same
			// output is already persisted in the caller's own transcript.
			pub("tool_end", map[string]any{"name": name, "isError": isErr, "output": clampToolOutput(output)})
			// Structured artifact lifting (files): the surface renders cards
			// from tool outputs, never model prose.
			liftFileArtifact(name, output, isErr, pub)
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
		OnCompact: func(cs agent.CompactStat) {
			// The transcript was durably rewritten (older turns condensed); tell
			// the user in-stream instead of letting history silently change.
			pub("compact", map[string]any{"kind": cs.Kind, "messagesBefore": cs.MessagesBefore, "messagesAfter": cs.MessagesAfter})
		},
		OnNavigate: func(d NavigateDirective) {
			pub("navigate", d)
			if cpmetrics.AssistantNavigationsTotal != nil {
				cpmetrics.AssistantNavigationsTotal.With().Inc()
			}
		},
		Confirm:          h.makeConfirm(p.userID, p.sessionID, pub),
		Redactor:         h.redactor, // scrub PII from tool output before prompt entry
		DisableBodyReads: h.cfg.DisableBodyReads,
	})
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
			// failure. An aborted turn emits turn_aborted (no error bubble).
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

	pub("done", map[string]any{"sessionId": p.sessionID})
}

// StreamSession is the long-lived SSE channel for a session's turn. It attaches to the
// bus, writes any events the client missed (replay from ?lastSeq=), then streams live
// events until the turn finishes or the client disconnects. A reconnect (network blip)
// re-opens this with the last seq it saw; the turn keeps running in the meantime, and
// a disconnect with no reconnect within the grace window cancels the turn (bus.detach).
func (h *Handler) StreamSession(c echo.Context) error {
	id := c.Param("id")
	if !validSessionID(id) {
		return writeErrJSON(c, http.StatusBadRequest, "validation_error", "a valid session id is required")
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
		return writeErrJSON(c, http.StatusNotFound, "not_found", "no active session stream")
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
		return writeErrJSON(c, http.StatusBadRequest, "validation_error", "a valid session id is required")
	}
	_, userID, ok := h.callerBearer(c)
	if !ok {
		return nil // callerBearer wrote the 422
	}
	if h.bus.interrupt(userID + ":" + id) {
		return c.NoContent(http.StatusNoContent)
	}
	return writeErrJSON(c, http.StatusConflict, "not_running", "no in-flight turn to stop for this session")
}

// toolOutputCap bounds the tool_end SSE payload. The chip is a peek, not an
// export: a full draft read (~11KB) or a long list would bloat every turn's
// stream and the widget DOM for no reading value. 4 KiB matches the
// device-event payload cap.
const toolOutputCap = 4 << 10

// clampToolOutput renders the (already redacted — the loop scrubs before the
// OnToolEnd peek) tool output for the SSE event, truncated on a rune boundary
// with an honest marker carrying the full size.
func clampToolOutput(b []byte) string {
	if len(b) <= toolOutputCap {
		return string(b)
	}
	// Trim to a rune START boundary (the excerptOf idiom): the byte AT the cap
	// must begin a rune, so a multi-byte rune the cap would split is dropped
	// whole. Bounded walk — output that is not UTF-8 at all (a binary read)
	// loses at most utf8.UTFMax-1 bytes, never the whole peek.
	cut := toolOutputCap
	for i := 0; i < utf8.UTFMax-1 && cut > 0 && !utf8.RuneStart(b[cut]); i++ {
		cut--
	}
	if !utf8.RuneStart(b[cut]) {
		cut = toolOutputCap // not a split rune — binary data; keep the full prefix
	}
	return fmt.Sprintf("%s\n… [truncated — %d bytes total]", b[:cut], len(b))
}
