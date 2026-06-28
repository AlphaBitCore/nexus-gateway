package assistant

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"github.com/goccy/go-json"
	"net/http"
	"sync"
	"time"

	cpmetrics "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/labstack/echo/v4"
)

// recordConfirm increments the dangerous-write gate decision counter
// decision ∈ {allow, deny, timeout, cancelled}. Nil-safe
// until cpmetrics.Register runs at startup.
func recordConfirm(decision string) {
	if cpmetrics.AssistantConfirmsTotal != nil {
		cpmetrics.AssistantConfirmsTotal.With(decision).Inc()
	}
}

// confirmDecisionLabel maps the resolved boolean to the metric decision label.
func confirmDecisionLabel(allowed bool) string {
	if allowed {
		return "allow"
	}
	return "deny"
}

// confirmTimeout bounds how long a confirm-tier tool waits for the user's decision
// before failing safe (deny). Long enough for a human to read the action, short
// enough never to wedge a turn forever.
const confirmTimeout = 2 * time.Minute

// impactTimeout bounds the impact-preview read so a slow/hung state read never
// delays the confirm card for long. On timeout (or any read error) the preview fails
// OPEN to an "unavailable" note — an emergency mitigation is never blocked by a
// degraded read.
const impactTimeout = 3 * time.Second

// confirmRegistry resolves confirm-tier decisions across HTTP requests: the detached
// turn goroutine (started by POST .../chat, observed over GET .../stream) parks on a
// channel; a separate POST /confirm delivers the decision by key
// (userID:sessionID:callID — the userID binding stops one principal from resolving
// another's parked write). In-memory per-process — multi-replica needs the session
// pinned to one pod (stream affinity). If the stream disconnects while a confirm is
// parked, the turn's disconnect-grace cancels its ctx → makeConfirm fail-safe denies.
//
// Production writes require a backend-enforced SECOND confirmation: an
// entry registered with requiresSecond=true (prod) is not unblocked by a single
// Allow. The first Allow makes the registry mint a one-time challenge token bound to
// the entry and keeps the turn parked; only a second Allow that echoes that exact
// token unblocks the write. A direct one-shot POST{decision:true} therefore cannot
// execute a prod write — the red button is no longer cosmetic.
type confirmEntry struct {
	ch             chan bool
	requiresSecond bool   // prod → a second token-bearing Allow is required
	challenge      string // one-time token minted on the first prod Allow; "" until then
}

type confirmRegistry struct {
	mu      sync.Mutex
	pending map[string]*confirmEntry
}

func newConfirmRegistry() *confirmRegistry {
	return &confirmRegistry{pending: make(map[string]*confirmEntry)}
}

func (r *confirmRegistry) register(key string, requiresSecond bool) chan bool {
	ch := make(chan bool, 1)
	r.mu.Lock()
	r.pending[key] = &confirmEntry{ch: ch, requiresSecond: requiresSecond}
	r.mu.Unlock()
	return ch
}

func (r *confirmRegistry) drop(key string) {
	r.mu.Lock()
	delete(r.pending, key)
	r.mu.Unlock()
}

// tryDrop removes a pending key and reports whether it was still present. It
// arbitrates the timeout-vs-resolve race: the loser (key already consumed by
// decide) reads the delivered decision instead of denying it.
func (r *confirmRegistry) tryDrop(key string) bool {
	r.mu.Lock()
	_, ok := r.pending[key]
	delete(r.pending, key)
	r.mu.Unlock()
	return ok
}

// confirmOutcome is the result of processing one POST /confirm.
type confirmOutcome int

const (
	confirmNotFound     confirmOutcome = iota // no parked turn — timed out / answered / unknown → 409
	confirmResolved                           // turn unblocked with the delivered decision → 200
	confirmChallenge                          // first prod Allow — token minted, write still parked → 200 + token
	confirmBadChallenge                       // prod second Allow with a wrong/empty token → 409 (no execute)
)

// decide processes one POST /confirm under a single lock so there is no
// check-then-act race with the timeout path. Semantics:
//   - Deny (decision=false): resolve immediately regardless of phase.
//   - Allow, non-prod: resolve immediately (single confirm).
//   - Allow, prod, no token: mint+store a one-time challenge token (idempotent — a
//     retried first Allow returns the same token), keep parked, return it.
//   - Allow, prod, with token: execute only if it matches the stored challenge.
//
// The decision is consumed once (the entry is deleted before the send), so a
// replayed or stale POST cannot double-execute a write.
func (r *confirmRegistry) decide(key string, decision bool, token string) (confirmOutcome, string) {
	r.mu.Lock()
	e, ok := r.pending[key]
	if !ok {
		r.mu.Unlock()
		return confirmNotFound, ""
	}
	if !decision { // deny — unblock immediately, any phase
		delete(r.pending, key)
		r.mu.Unlock()
		e.ch <- false
		return confirmResolved, ""
	}
	if !e.requiresSecond { // non-prod allow — single confirm
		delete(r.pending, key)
		r.mu.Unlock()
		e.ch <- true
		return confirmResolved, ""
	}
	if token == "" { // prod first allow — mint the challenge, stay parked
		if e.challenge == "" {
			e.challenge = newToken()
		}
		ch := e.challenge
		r.mu.Unlock()
		return confirmChallenge, ch
	}
	if e.challenge == "" || token != e.challenge { // prod second allow — bad token
		r.mu.Unlock()
		return confirmBadChallenge, ""
	}
	delete(r.pending, key) // prod second allow — token matches → execute
	r.mu.Unlock()
	e.ch <- true
	return confirmResolved, ""
}

func newCallID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// newToken mints the one-time prod second-confirm challenge. 16 bytes of crypto
// randomness — unguessable, so a client cannot satisfy the second confirm without
// having received this exact token in the first Allow's response.
func newToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// makeConfirm builds the kernel ConfirmFunc for one turn. On a confirm-tier tool it
// emits a `confirm` SSE event — the content contract: tool name + the resolved
// structured input + reason, server-rendered (never model free-text) so an injected
// payload cannot forge the human-facing prompt — then parks until POST /confirm
// resolves it, the turn ctx cancels, or the timeout fires (all → fail-safe deny).
func (h *Handler) makeConfirm(userID, sessionID string, send func(event string, payload any)) agent.ConfirmFunc {
	// Mirror each parked confirm to a durable row so a POST /confirm after a
	// restart reads as "re-issue" rather than "expired". nil when persistence is absent.
	pstore := newPendingConfirmStore(h.cfg.Pool, userID)
	return func(ctx context.Context, tool agent.Tool, input json.RawMessage, reason string) (bool, error) {
		callID := newCallID()
		// Key is bound to the authenticated userId so a POST /confirm from a different
		// principal can never resolve this user's parked write.
		key := userID + ":" + sessionID + ":" + callID
		// In production every confirm-tier tool requires a backend-enforced second
		// confirmation: the entry is registered as requiresSecond so a single
		// Allow only mints a challenge token; the write executes on the second Allow.
		ch := h.confirms.register(key, h.cfg.IsProd)
		// Persist the parked confirm + remove it on ANY resolution (resolve / deny /
		// timeout / cancel). A normal flow leaves no row; only a restart orphans one.
		// The delete uses a detached ctx so a cancelled turn ctx still cleans up.
		pstore.put(ctx, key, sessionID, callID, tool.Name(), input, reason, h.cfg.IsProd, h.cfg.IsProd)
		defer func() {
			dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Second)
			pstore.del(dctx, key)
			dcancel()
		}()
		payload := map[string]any{
			"callId":    callID,
			"sessionId": sessionID,
			"tool":      tool.Name(),
			"input":     input,
			"reason":    reason,
			"prod":      h.cfg.IsProd,
		}
		// For high-blast-radius tools, surface a structured impact preview
		// (current → effect) in the card BEFORE the operator can Allow. Computed
		// server-side from the tool's read-only ImpactDetailer; absent for ordinary
		// confirm tools, and fail-open to an "unavailable" note if the read errors.
		if preview := h.impactPreview(ctx, tool, input); preview != nil {
			payload["preview"] = preview
		}
		send("confirm", payload)
		select {
		case ok := <-ch:
			recordConfirm(confirmDecisionLabel(ok))
			return ok, nil
		case <-ctx.Done():
			h.confirms.drop(key)
			recordConfirm("cancelled")
			return false, ctx.Err()
		case <-time.After(h.confirmTimeout):
			// Honor a decision that raced in exactly at the deadline: only deny if
			// the confirm is still genuinely pending (tryDrop removed it). If a
			// POST /confirm already resolved it (tryDrop false), take that delivered
			// decision from the buffered channel rather than reporting a false deny.
			if h.confirms.tryDrop(key) {
				recordConfirm("timeout") // genuinely timed out → fail-safe deny
				return false, nil
			}
			ok := <-ch
			recordConfirm(confirmDecisionLabel(ok))
			return ok, nil
		}
	}
}

// impactPreview computes the impact preview for a confirm-tier tool, or nil
// when the tool has none. The read is bounded by impactTimeout and read-only; on any
// error or timeout it fails OPEN — returning an "unavailable" marker rather than nil,
// so a high-blast-radius tool still shows a card (and the operator a caution) instead
// of an emergency mitigation being blocked by a degraded state read.
func (h *Handler) impactPreview(ctx context.Context, tool agent.Tool, input json.RawMessage) any {
	d, ok := tool.(agent.ImpactDetailer)
	if !ok {
		return nil
	}
	ictx, cancel := context.WithTimeout(ctx, h.impactTimeout)
	defer cancel()
	preview, err := d.ImpactDetail(ictx, input)
	if err != nil {
		return map[string]any{
			"unavailable": true,
			"note":        "Impact preview could not be computed (state read failed); proceed with caution.",
		}
	}
	return preview // may be nil → ordinary confirm tool, no preview field
}

// Confirm resolves a parked confirm-tier tool. The decision is consumed once (CAS in
// the registry); a stale/duplicate/unknown call gets 409 so a write never
// double-executes. The caller is already authenticated by the admin group.
//
// Production second confirm: on a prod instance an Allow with no
// challengeToken does NOT execute — the backend mints a one-time token, returns it as
// {secondConfirmRequired:true, challengeToken}, and keeps the write parked. The
// client must POST a second Allow echoing that token; only then does the write run.
// A wrong/missing token on the second step is a 409 with no execution. Deny ends the
// turn at any phase. This makes prod gating backend-enforced, not a cosmetic button.
func (h *Handler) Confirm(c echo.Context) error {
	var body struct {
		SessionID      string `json:"sessionId"`
		CallID         string `json:"callId"`
		Decision       bool   `json:"decision"`
		ChallengeToken string `json:"challengeToken"`
	}
	if err := c.Bind(&body); err != nil || body.SessionID == "" || body.CallID == "" {
		return writeErrJSON(c, http.StatusBadRequest, "validation_error", "sessionId and callId are required")
	}
	userID := ""
	if aa := middleware.AdminAuthFromContext(c); aa != nil {
		userID = aa.KeyID
	}
	key := userID + ":" + body.SessionID + ":" + body.CallID

	outcome, token := h.confirms.decide(key, body.Decision, body.ChallengeToken)
	// Local-first: if the confirm was parked HERE, decide already resolved it
	// regardless of what the owner registry says (avoids a race where a newer turn
	// re-claimed ownership while this confirm was parked). Only when it is NOT here
	// do we consult the registry: if another live instance owns this session, the
	// parked channel lives there — answer 421 so the LB / client retries at the
	// owner instead of getting a misleading 409. Fail-open: an unknown owner (no
	// Redis / key expired / transport error) falls through to the normal outcome.
	if outcome == confirmNotFound {
		if mine, known := h.owners.owner(c.Request().Context(), userID+":"+body.SessionID); known && !mine {
			if cpmetrics.AssistantConfirmMisrouteTotal != nil {
				cpmetrics.AssistantConfirmMisrouteTotal.With().Inc()
			}
			return writeErrJSON(c, http.StatusMisdirectedRequest, "wrong_owner",
				"this session is being handled by another instance; retry the request")
		}
	}
	switch outcome {
	case confirmResolved:
		return c.JSON(http.StatusOK, map[string]any{"ok": true})
	case confirmChallenge:
		return c.JSON(http.StatusOK, map[string]any{
			"ok":                    true,
			"secondConfirmRequired": true,
			"challengeToken":        token,
		})
	case confirmBadChallenge:
		return writeErrJSON(c, http.StatusConflict, "invalid_challenge",
			"the production second-confirm token is missing or no longer valid; re-issue the action")
	default: // confirmNotFound
		// An in-memory miss with a still-fresh durable row means the pod that
		// parked this confirm restarted (the in-memory channel + turn goroutine are
		// gone) — tell the user to re-issue rather than the confusing "expired".
		// NOTE: this is reached only after the owner-registry 421 check above. After a
		// real pod CRASH the dead pod's Redis ownership key lingers until its TTL, so a
		// misrouted confirm gets 421 (wrong_owner) until the TTL lapses, then falls
		// through to this re-issue verdict. Both tell the user the action did not run;
		// the 421 is correct while Redis still believes a live owner holds the session.
		if ps := newPendingConfirmStore(h.cfg.Pool, userID); ps.fresh(c.Request().Context(), key) {
			return writeErrJSON(c, http.StatusConflict, "restart_reissue",
				"the assistant restarted before this action completed; please re-issue it")
		}
		return writeErrJSON(c, http.StatusConflict, "expired",
			"no pending confirmation for that call (it may have timed out or already been answered)")
	}
}
