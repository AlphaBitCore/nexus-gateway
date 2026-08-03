package proxy

// realtime_session.go — the live-session half of the realtime relay: the
// session state shared by both pumps, the supervision loop (client liveness
// ping, VK status recheck, hard session guard), and the exactly-once
// teardown that every close path — peer close, policy sever, quota sever,
// guard expiry, relay error, panic unwind — funnels through.

import (
	"context"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/realtimeproxy"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// realtimeQuota is the narrow quota surface the per-response settlement
// uses. *quota.Engine satisfies it; tests substitute a fake to pin the
// fresh-Check-then-Reconcile mechanics without Redis.
type realtimeQuota interface {
	OrgParents() map[string]string
	Check(ctx context.Context, chain []quota.CheckLevel, estimate quota.CostEstimate, vkMeta *vkauth.VKMeta) *quota.Decision
	Reconcile(ctx context.Context, decision *quota.Decision, actual quota.ActualUsage)
}

// realtimeSession carries one live relay session. Both pumps, the supervisor
// goroutine, and the finalize path share it; every mutable member is either
// owned by exactly one goroutine, internally synchronized (SessionState), or
// guarded by closeOnce.
type realtimeSession struct {
	h       *Handler
	rec     *audit.Record
	in      Ingress
	vkMeta  *vkauth.VKMeta
	keyHash string
	// rechecker is nil when the wired authenticator lacks the by-hash seam
	// (narrow test doubles); the 65-minute guard still bounds such sessions.
	rechecker vkStatusRechecker
	quota     realtimeQuota

	sessionID  string
	target     routingcore.RoutingTarget
	callTarget provcore.CallTarget
	// priceModel is the catalog pricing snapshot pinned at upgrade (nil when
	// the lookup failed — every component then prices $0 with the deduped
	// WARN; the enforced-quota arm was refused pre-upgrade).
	priceModel *store.Model
	prices     metrics.ModelPrices

	client   *websocket.Conn
	upstream *websocket.Conn

	state      *realtimeproxy.SessionState
	frameLimit int64

	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// newRealtimeSession assembles a session after a successful dial + accept.
func newRealtimeSession(
	h *Handler,
	rec *audit.Record,
	in Ingress,
	vkMeta *vkauth.VKMeta,
	keyHash string,
	target routingcore.RoutingTarget,
	callTarget provcore.CallTarget,
	priceModel *store.Model,
	sessionID string,
	client, upstream *websocket.Conn,
	frameLimit int64,
) *realtimeSession {
	s := &realtimeSession{
		h:          h,
		rec:        rec,
		in:         in,
		vkMeta:     vkMeta,
		keyHash:    keyHash,
		sessionID:  sessionID,
		target:     target,
		callTarget: callTarget,
		priceModel: priceModel,
		prices:     realtimeModelPrices(priceModel),
		client:     client,
		upstream:   upstream,
		state:      &realtimeproxy.SessionState{},
		frameLimit: frameLimit,
	}
	if h.deps.QuotaEngine != nil {
		s.quota = h.deps.QuotaEngine
	}
	return s
}

// run relays until the session closes, then returns so the handler can stamp
// and enqueue the session row. It blocks the handler goroutine on purpose:
// the deferred gate/caps/audit tail in ServeRealtime is the finalize path.
func (s *realtimeSession) run(reqCtx context.Context) {
	// The session context deliberately detaches from any request-scoped
	// cancel/deadline armed before the hijack — a middleware deadline must
	// not sever a 65-minute voice session. Liveness is the relay's own job:
	// ping/pong, the VK recheck, and the hard guard below.
	s.ctx, s.cancel = context.WithCancel(context.WithoutCancel(reqCtx))
	defer s.cancel()

	realtimeSessionsActive.Inc()
	defer realtimeSessionsActive.Dec()

	// Per-frame ceiling on BOTH connections: the per-session memory budget
	// is fixed at ≤ 2 × frameLimit (one in-flight frame per direction; the
	// pumps hold no queue). -1 = unlimited (env override, warned at
	// admission).
	s.client.SetReadLimit(s.frameLimit)
	s.upstream.SetReadLimit(s.frameLimit)

	s.wg.Add(2)
	go s.pump(s.client, s.upstream, false)
	go s.pump(s.upstream, s.client, true)
	var superviseDone sync.WaitGroup
	superviseDone.Add(1)
	go func() {
		defer superviseDone.Done()
		s.supervise()
	}()

	s.wg.Wait()
	// Both pumps have exited; make sure the close handshake ran even on a
	// path that returned without an explicit teardown call, then wait for
	// the supervisor — no session goroutine may outlive the handler (its
	// deferred tail releases the gate and caps slots this session holds).
	s.teardown(realtimeproxy.ReasonNormal, websocket.StatusNormalClosure)
	superviseDone.Wait()
}

// supervise runs the session timers: client liveness ping, periodic VK
// status recheck, and the hard session guard. It exits when the session
// context is canceled by teardown.
func (s *realtimeSession) supervise() {
	// A panic in supervision (the recheck touches the VK cache/DB) must tear
	// down this session only, never the process.
	defer func() {
		if r := recover(); r != nil {
			if s.h.deps.Logger != nil {
				s.h.deps.Logger.Error("realtime supervisor panic", "panic", r)
			}
			s.teardown(realtimeproxy.ReasonRelayError, websocket.StatusInternalError)
		}
	}()

	ping := time.NewTicker(realtimePingInterval)
	defer ping.Stop()
	recheck := time.NewTicker(realtimeRecheckInterval)
	defer recheck.Stop()
	guard := time.NewTimer(realtimeSessionGuard)
	defer guard.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ping.C:
			pctx, cancel := context.WithTimeout(s.ctx, realtimePongTimeout)
			err := s.client.Ping(pctx)
			cancel()
			if err != nil {
				if s.ctx.Err() != nil {
					return
				}
				// Dead client: free the session (and its gate/caps slots)
				// instead of holding them until the guard.
				s.teardown(realtimeproxy.ReasonClientClosed, websocket.StatusNormalClosure)
				return
			}
		case <-recheck.C:
			if s.recheckVK() {
				return
			}
		case <-guard.C:
			// The provider maximum is 60 minutes; a session still alive at
			// 65 is wedged. Close it so gate + caps slots cannot be pinned
			// forever.
			s.sendClientErrorEvent("REALTIME_SESSION_GUARD", "session exceeded the maximum duration")
			s.teardown(realtimeproxy.ReasonSessionGuard, websocket.StatusNormalClosure)
			return
		}
	}
}

// recheckVK re-validates the VK by its matched hash. Returns true when the
// session was severed. Only a DEFINITIVE negative severs; an indeterminate
// lookup error fails OPEN (counter + retry next tick) — a transient backend
// blip must not simultaneously kill every live voice session, and the
// 65-minute guard bounds the worst case.
func (s *realtimeSession) recheckVK() bool {
	if s.rechecker == nil || s.keyHash == "" {
		return false
	}
	rctx, cancel := context.WithTimeout(s.ctx, realtimeRecheckTimeout)
	outcome, err := s.rechecker.RecheckByHash(rctx, s.keyHash)
	cancel()
	if err != nil {
		// Indeterminate (incl. a timeout on a wedged backend): fail open,
		// count it, retry next tick. The guard bounds the worst case.
		realtimeVKRecheckFailures.Inc()
		return false
	}
	if outcome != vkauth.RecheckRevoked {
		return false
	}
	s.sendClientErrorEvent("REALTIME_VK_REVOKED", "virtual key is no longer active")
	s.teardown(realtimeproxy.ReasonVKRevoked, websocket.StatusPolicyViolation)
	return true
}

// teardown closes both connections exactly once and cancels the session
// context. The FIRST caller's reason wins the session row's close-reason
// stamp (SessionState.SetCloseReason is first-writer-wins), and the first
// caller's status code goes on both close frames — the originating cause
// must not be overwritten by the cascade it triggers.
func (s *realtimeSession) teardown(reason realtimeproxy.CloseReason, code websocket.StatusCode) {
	s.state.SetCloseReason(reason)
	s.closeOnce.Do(func() {
		_, winner, _ := s.state.Snapshot()
		note := winner.ErrorCode()
		if note == "" {
			note = "session closed"
		}
		_ = s.client.Close(code, note)
		_ = s.upstream.Close(code, note)
		s.cancel()
	})
}

// sendClientErrorEvent writes one OpenAI-shaped error event to the client
// before a policy/guard/upstream-loss close, so SDK callers surface a
// machine-readable reason rather than a bare close frame. Best-effort with a
// short bound — a dead client must not stall the teardown behind the full
// relay write deadline.
func (s *realtimeSession) sendClientErrorEvent(code, message string) {
	evt := map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "gateway_error",
			"code":    code,
			"message": message,
		},
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
	defer cancel()
	_ = s.client.Write(wctx, websocket.MessageText, payload)
}
