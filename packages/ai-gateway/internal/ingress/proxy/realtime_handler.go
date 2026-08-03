package proxy

// realtime_handler.go — the realtime voice WebSocket relay handler for
// GET /v1/realtime (OpenAI Realtime API, WebSocket transport).
//
// Realtime is a parallel handler like STT and video — a long-lived
// bidirectional connection cannot ride the small-JSON ServeProxy pipeline
// (request hooks, cache, codec, and the byte-slice executor are all
// per-request constructs). The handler reuses the SAME cross-cutting subset
// (in-flight gate, VK auth, per-VK RPM, generative-caps session concurrency,
// router + credential resolve, quota engine, cost estimator, audit writer)
// via the shared Handler deps and touches NONE of ServeProxy's internals.
//
// The relay is dark-launched behind an INVERTED entitlement gate: a VK is
// admitted only when its AllowedModels list explicitly names the requested
// realtime model. An EMPTY list — "unrestricted" everywhere else — is NOT
// entitled here, because an unbounded voice session is the most expensive
// billable surface the gateway serves and must be a positive opt-in.
// The refusal is 404 NO_COMPATIBLE_PROVIDER (non-disclosure: same envelope
// as an unroutable model, so a probe cannot distinguish "exists but not
// entitled" from "does not exist").

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/realtimeproxy"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/generativecaps"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// Session timing knobs. Vars, not consts, so tests can shorten them (the Hub
// ws pingInterval idiom); production code must not mutate them.
var (
	// realtimePingInterval / realtimePongTimeout drive client liveness: a
	// dead client frees its session (and its gate + caps slots) instead of
	// holding them until the guard. The upstream library auto-answers
	// provider pings during Read, so only the client side needs an active
	// ping loop. The pong timeout MUST exceed realtimeWriteTimeout: the WS
	// library processes an inbound pong only while some goroutine is in Read,
	// and the client-side reader (the client→upstream pump) can be parked up
	// to realtimeWriteTimeout inside a large-frame Write to a slow upstream —
	// a shorter pong timeout would false-kill a healthy session whenever a
	// ping tick lands mid-write.
	realtimePingInterval = 30 * time.Second
	realtimePongTimeout  = 35 * time.Second
	// realtimeRecheckInterval is the by-hash VK status re-validation cadence;
	// realtimeRecheckTimeout bounds a single recheck so a wedged VK-cache/DB
	// backend cannot freeze the supervisor (and with it the liveness ping and
	// the hard guard) — the same defense the ping arm already applies.
	realtimeRecheckInterval = 60 * time.Second
	realtimeRecheckTimeout  = 5 * time.Second
	// realtimeSessionGuard hard-closes any session. The provider maximum is
	// 60 minutes; the guard exists so a wedged upstream cannot pin gate and
	// caps slots forever. Built-in constant posture, not an admin knob.
	realtimeSessionGuard = 65 * time.Minute
)

const (
	// realtimeWriteTimeout bounds each relay frame write. Deliberately NOT
	// the Hub's 5 s control-frame deadline: a full-size audio frame at 5 s
	// would demand ~27 Mbit/s sustained client downlink and kill healthy
	// sessions on exactly the frames that matter most; 30 s tolerates a
	// 16 MiB frame at ~4.5 Mbit/s, and dead peers are detected by ping/pong
	// independently, so the longer deadline does not delay dead-session
	// cleanup.
	realtimeWriteTimeout = 30 * time.Second
	// Pre-accept dial budget: at most 3 targets, 8 s per attempt, 24 s
	// overall. The upgrade request's server WriteTimeout (armed when the
	// request was read, cleared only by a successful hijack) must still have
	// room to write the 502 if every dial fails, and 24 s stays inside
	// typical WebSocket client handshake timeouts.
	realtimeDialAttemptTimeout = 8 * time.Second
	realtimeDialOverallTimeout = 24 * time.Second
	realtimeMaxDialTargets     = 3
)

// realtimeSessionsActive gauges live relay sessions; realtimeVKRecheckFailures
// counts INDETERMINATE by-hash recheck errors (the fail-open arm — a definitive
// revocation severs and is visible on the session row instead).
var (
	realtimeSessionsActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "nexus_realtime_sessions_active",
		Help: "Live realtime voice relay WebSocket sessions.",
	})
	realtimeVKRecheckFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nexus_realtime_vk_recheck_failures_total",
		Help: "Indeterminate VK status recheck failures during realtime sessions (failed open; retried next tick).",
	})
)

// vkStatusRechecker is the optional seam a VK authenticator implements to
// support long-lived sessions: authenticate returning the matched key hash,
// and later re-validate status by that hash alone. *vkauth.Authenticator
// implements it; narrow test doubles that don't are served without periodic
// recheck (the 65-minute guard still bounds them).
type vkStatusRechecker interface {
	AuthenticateWithHash(ctx context.Context, r *http.Request) (*vkauth.VKMeta, string, error)
	RecheckByHash(ctx context.Context, keyHash string) (vkauth.RecheckOutcome, error)
}

// warnRealtimeUnlimitedFrames dedupes the loud WARN for an env-overridden
// frame ceiling of 0 (= unlimited — a deliberate operator choice, but a
// memory-DoS surface worth one loud line per process).
var warnRealtimeUnlimitedFrames sync.Once

// ServeRealtime returns the HTTP handler for the realtime voice relay route.
// It runs the full admission chain on the plain-HTTP upgrade request (every
// refusal is an ordinary HTTP error response), dials the provider FIRST, and
// only then hijacks the client connection; the session relay itself blocks
// until close, so the handler's deferred tail is the panic-safe finalize path
// for the gate slot, the caps slot, and the $0 session audit row.
func (h *Handler) ServeRealtime(in Ingress) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.gate != nil {
			if !h.gate.acquire() {
				writeOverloaded(w, in.BodyFormat)
				return
			}
			defer h.gate.release()
		}

		start := time.Now().UTC()
		// The grouping key for ALL of the session's traffic_event rows is a
		// SERVER-minted UUID. It must never derive from the request's id or
		// trace headers: the middleware honors an inbound X-Nexus-Request-Id,
		// so a client could otherwise merge or collide session groups.
		sessionID := uuid.NewString()
		rec := &audit.Record{
			RequestID:       r.Header.Get("X-Nexus-Request-Id"),
			ClientRequestID: r.Header.Get("x-request-id"),
			TraceID:         sessionID,
			Timestamp:       start,
			Method:          r.Method,
			Path:            r.URL.Path,
			SourceIP:        middleware.ClientIP(r),
			IngressFormat:   string(in.BodyFormat),
			EndpointType:    string(typology.EndpointKindRealtime),
			// No content scanning runs in either direction on this relay;
			// every row says so honestly.
			ComplianceCoverage: "none",
		}

		// Panic-safe finalize tail: caps release + session row enqueue run on
		// EVERY exit path — pre-upgrade refusals, dial failures, session
		// close, and panic unwind. A leaked session slot would wedge the
		// per-VK cap for the process lifetime.
		var releaseCap func()
		defer func() {
			if releaseCap != nil {
				releaseCap()
			}
			if h.deps.AuditWriter != nil {
				h.finalize(rec, start)
			}
		}()

		// WebSocket preconditions. A non-upgrade request (or a missing
		// ?model=) is a malformed call, refused before any credential work.
		if !headerHasToken(r.Header.Get("Upgrade"), "websocket") {
			h.writeDetailedErr(w, rec, http.StatusBadRequest, "REALTIME_UPGRADE_REQUIRED",
				"this endpoint serves WebSocket upgrades only",
				"Connect with a WebSocket client (Upgrade: websocket)")
			return
		}
		requestedModel := r.URL.Query().Get("model")
		if requestedModel == "" {
			h.writeDetailedErr(w, rec, http.StatusBadRequest, "REALTIME_MODEL_REQUIRED",
				"the model query parameter is required",
				"Connect to /v1/realtime?model=<model>")
			return
		}

		// VK auth. When the authenticator has the long-session seam, retain
		// the MATCHED key hash for the periodic status recheck — never the
		// raw bearer token or the inbound request (holding a live credential
		// in process for a 65-minute session is not acceptable).
		var (
			vkMeta  *vkauth.VKMeta
			keyHash string
			err     error
		)
		rechecker, hasRecheck := h.deps.VKAuth.(vkStatusRechecker)
		if hasRecheck {
			vkMeta, keyHash, err = rechecker.AuthenticateWithHash(r.Context(), r)
		} else {
			vkMeta, err = h.authenticate(r)
		}
		if err != nil {
			h.writeAuthError(w, rec, err)
			return
		}
		rec.ApplyVKMeta(vkMeta)
		rec.APIKeyClass = vkMeta.Class
		rec.APIKeyFingerprint = vkMeta.Fingerprint

		// Per-VK RPM: the upgrade counts as ONE request. Per-turn RPM does
		// not exist on a socket and is not simulated.
		if rlErr := h.checkRateLimit(w, vkMeta); rlErr != nil {
			h.writeDetailedErr(w, rec, http.StatusTooManyRequests, "RATE_LIMITED",
				"per-key rate limit exceeded", "Reduce request rate or retry after the Retry-After interval")
			return
		}

		// Generative-caps SESSION concurrency: the slot is held for the
		// session's whole life and released by the finalize tail. The realtime
		// row is a load-bearing DoS floor (per-VK session count + per-frame
		// memory); if it is ever missing from the registry, fall back to the
		// built-in default rather than fail OPEN to unlimited sessions AND
		// unlimited frames at once.
		caps, generative := generativecaps.Lookup(typology.EndpointKindRealtime)
		if !generative {
			caps = generativecaps.Caps{MaxConcurrentPerVK: 2, MaxRequestBytes: 16 << 20}
		}
		if caps.MaxConcurrentPerVK > 0 && h.genConcurrency != nil {
			if !h.genConcurrency.Acquire(typology.EndpointKindRealtime, vkMeta.ID, caps.MaxConcurrentPerVK) {
				generativeCapShedTotal.WithLabelValues(string(typology.EndpointKindRealtime)).Inc()
				// Retry-After MUST precede the error writer's header commit.
				w.Header().Set("Retry-After", "1")
				h.writeDetailedErr(w, rec, http.StatusTooManyRequests, "GENERATIVE_CONCURRENCY_LIMIT",
					"too many concurrent realtime sessions for this key",
					"Close a session or retry shortly")
				return
			}
			vkID := vkMeta.ID
			releaseCap = func() { h.genConcurrency.Release(typology.EndpointKindRealtime, vkID) }
		}
		// For realtime, MaxRequestBytes is the per-WS-FRAME ceiling enforced
		// via SetReadLimit on BOTH connections (the upgrade has no body). An
		// env override of 0 means UNLIMITED frames — honored (operator
		// choice) but loud: unbounded frames are a memory-DoS surface.
		frameLimit := caps.MaxRequestBytes
		if frameLimit <= 0 {
			warnRealtimeUnlimitedFrames.Do(func() {
				if h.deps.Logger != nil {
					h.deps.Logger.Warn("realtime frame ceiling override is 0: per-frame read limit DISABLED — unbounded frames are a memory-DoS surface")
				}
			})
			frameLimit = -1 // SetReadLimit(-1) = unlimited
		}

		// The dark-launch entitlement gate (inverted AllowedModels
		// semantics), then normal routing. The model pre-resolve fails CLOSED
		// — "can't resolve refs" is never "skip entitlement".
		if h.deps.Models == nil {
			h.writeRealtimeNoProvider(w, rec)
			return
		}
		catModel, catErr := h.deps.Models.GetModelByCodeOrAlias(r.Context(), requestedModel)
		if catErr != nil || catModel == nil {
			h.writeRealtimeNoProvider(w, rec)
			return
		}
		if len(vkMeta.AllowedModels) == 0 ||
			!routingcore.ModelMatchesAllowedRefs(catModel.ID, catModel.ProviderModelID, catModel.ProviderID, vkMeta.AllowedModels) {
			h.writeRealtimeNoProvider(w, rec)
			return
		}

		// Routing (rules, strategy, health) exactly as every endpoint, then
		// narrow to OpenAI-format targets — the dial derivation and auth
		// injection are OpenAI-shaped; other formats are not relayable here.
		rctx := h.buildRequestContext(r, vkMeta, nil, in.BodyFormat, requestedModel, string(typology.EndpointKindRealtime))
		routeRes, routeErr := h.resolveRouteOrPassthrough(r.Context(), rctx, in, requestedModel, typology.EndpointKindRealtime)
		if routeErr != nil || routeRes == nil || len(routeRes.Targets) == 0 {
			h.writeRealtimeNoProvider(w, rec)
			return
		}
		targets := filterRealtimeTargets(routeRes.Targets)
		if len(targets) == 0 {
			h.writeRealtimeNoProvider(w, rec)
			return
		}

		// VK-scoped advisory quota check (model-INDEPENDENT), before any dial:
		// a strictly-exceeded enforced level refuses the upgrade fast, without
		// opening an upstream connection. Zero estimate — a session's cost is
		// unknowable at upgrade, so there is no estimate-as-floor and no
		// admission debit; metering is per-response settlement. An
		// exactly-at-limit VK admits and is refused by the per-response
		// evaluation after its first response (documented boundary, bounded to
		// one response per session slot). The decision is reused post-dial for
		// the pinned model's pricing-completeness gate.
		var quotaDecision *quota.Decision
		if h.deps.QuotaEngine != nil {
			chain := quota.BuildCheckChain(vkMeta, h.deps.QuotaEngine.OrgParents())
			quotaDecision = h.deps.QuotaEngine.Check(r.Context(), chain, quota.CostEstimate{}, vkMeta)
			if quotaDecision != nil && !quotaDecision.Allowed {
				h.writeDetailedErr(w, rec, http.StatusTooManyRequests, "QUOTA_EXCEEDED",
					quotaDecision.Message, "Check usage or request a quota increase")
				return
			}
		}

		// Dial the provider FIRST; only a successful upstream handshake earns
		// the client its 101 (a 101-then-immediate-close would break every
		// client SDK's error handling). The dial PINS one target — a
		// connection-time failover may pin targets[1..2], so ALL attribution
		// and the pricing-completeness gate below key off the pinned target,
		// never targets[0]; otherwise a failover would mislabel every row and
		// evaluate the $0-audio quota gate against the wrong model.
		upstream, pinned, callTarget, ok := h.dialRealtimeUpstream(w, r, rec, targets, vkMeta.ID)
		if !ok {
			return // static 502 already written
		}
		rec.ModelID = pinned.ModelID
		rec.ModelName = pinned.ModelName
		rec.RoutedModelID = pinned.ModelID
		rec.RoutedModelName = pinned.ModelName
		rec.RoutedProviderID = pinned.ProviderID
		rec.RoutedProviderName = pinned.ProviderName
		rec.ProviderID = callTarget.ProviderID
		rec.ProviderName = callTarget.ProviderName
		rec.CredentialID = callTarget.CredentialID
		rec.CredentialName = callTarget.CredentialName
		rec.TargetMethod = http.MethodGet
		rec.TargetPath = "/v1/realtime"
		rec.TargetHost = hostOf(callTarget.BaseURL)

		// Pricing completeness for the PINNED model. Nil OR literal-zero
		// primary rates both under-price real spend to $0 (the completeness
		// contract is non-nil AND > 0). Not-realtime-priced under an actively
		// enforced cost cap fails closed — a $0-metered audio session is the
		// exact hole QUOTA_MODEL_UNPRICED closes; the dialed upstream is
		// closed so no orphan accrues spend. 503, not 429: this is operator
		// misconfiguration (a missing price row), not caller-correctable
		// backoff.
		priceModel, realtimePriced := h.realtimePricing(r.Context(), pinned.ModelID)
		if !realtimePriced {
			rec.Metadata = stampUnpricedCost(rec.Metadata)
			if quotaHasCostLimit(quotaDecision) {
				_ = upstream.Close(websocket.StatusGoingAway, "model not realtime-priced")
				if h.deps.Logger != nil {
					h.deps.Logger.Warn("realtime quota: pinned model is not realtime-priced; rejecting under an active cost quota",
						"model", pinned.ModelID, "vk", vkMeta.ID)
				}
				h.writeDetailedErr(w, rec, http.StatusServiceUnavailable, "QUOTA_MODEL_UNPRICED",
					"routed model has no complete realtime pricing; cost quota cannot be enforced",
					"Ask an admin to set the model's text and audio token prices")
				return
			}
		}

		// Accept (hijack). No OriginPatterns: server clients send no Origin
		// and a browser Origin is rejected by default, consistent with the
		// server-clients-only auth model. No subprotocols offered or echoed.
		client, acceptErr := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if acceptErr != nil {
			// Accept wrote its own HTTP error. Close the dialed provider
			// session so no orphan accrues audio-token spend.
			_ = upstream.Close(websocket.StatusGoingAway, "client accept failed")
			rec.StatusCode = http.StatusInternalServerError
			rec.ErrorCode = "REALTIME_ACCEPT_FAILED"
			rec.ErrorReason = "websocket accept failed"
			return
		}

		sess := newRealtimeSession(h, rec, in, vkMeta, keyHash, pinned, callTarget,
			priceModel, sessionID, client, upstream, frameLimit)
		if hasRecheck && keyHash != "" {
			sess.rechecker = rechecker
		}
		sess.run(r.Context()) // blocks until the session closes

		// Session row: $0 / zero tokens (all cost lives on response rows),
		// StatusCode 101 (the upgrade succeeded; there is no HTTP status for
		// how a live WS ended), LatencyMs = 1 — an explicit floor, NEVER the
		// session duration, which would drag the shipped latency percentiles
		// by six orders of magnitude. Duration, response count, and close
		// reason ride the metadata field instead.
		responses, reason, errCode := sess.state.Snapshot()
		rec.StatusCode = http.StatusSwitchingProtocols
		rec.LatencyMs = 1
		if errCode != "" {
			rec.ErrorCode = errCode
		}
		rec.Metadata = stampRealtimeSession(rec.Metadata,
			time.Since(start).Milliseconds(), responses, reason)
	}
}

// headerHasToken reports whether a comma-separated header value contains the
// given token, case-insensitively (the Upgrade header is a token list).
func headerHasToken(headerValue, token string) bool {
	for _, part := range strings.Split(headerValue, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// writeRealtimeNoProvider is the single refusal envelope for every
// not-routable arm — unknown model, resolution failure, non-entitled VK, no
// OpenAI-format target. One envelope by design: a probing caller must not be
// able to distinguish "model exists but you are not entitled" from "model
// does not exist".
func (h *Handler) writeRealtimeNoProvider(w http.ResponseWriter, rec *audit.Record) {
	h.writeDetailedErr(w, rec, http.StatusNotFound, "NO_COMPATIBLE_PROVIDER",
		"no provider is configured to serve this realtime model",
		"Check the model name, or ask an operator to configure realtime access for this key")
}

// filterRealtimeTargets keeps only OpenAI-wire targets in routed order. The
// wss URL derivation and header auth injection speak the OpenAI realtime
// convention; a non-OpenAI target cannot be relayed by this handler.
func filterRealtimeTargets(targets []routingcore.RoutingTarget) []routingcore.RoutingTarget {
	out := make([]routingcore.RoutingTarget, 0, len(targets))
	for _, t := range targets {
		if strings.EqualFold(t.AdapterType, "openai") {
			out = append(out, t)
		}
	}
	return out
}

// stampRealtimeSession writes the session accounting (duration, response
// count, close reason) into the session row's metadata JSONB — deliberately
// NOT into LatencyMs or the token/cost columns, which keep their
// everywhere-else meanings.
func stampRealtimeSession(existing any, durationMs int64, responses int, reason realtimeproxy.CloseReason) any {
	label := reason.ErrorCode()
	if label == "" {
		label = "normal"
	}
	md := mergeIntoMetadataMap(existing)
	md["realtime"] = map[string]any{
		"sessionDurationMs": durationMs,
		"responses":         responses,
		"closeReason":       label,
	}
	return md
}
