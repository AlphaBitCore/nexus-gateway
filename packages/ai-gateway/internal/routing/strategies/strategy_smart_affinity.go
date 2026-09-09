// strategy_smart_affinity.go — keep one conversation on one model while the
// provider's prompt cache for it is still warm.
package strategies

import (
	"context"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// A provider's prompt cache is keyed on a per-model hash of the request prefix.
// A conversation that leaves a model and comes back finds a prefix that now
// contains an assistant turn produced by a different model, so the hash differs
// and the entry it paid 1.25x to create is dead. Staging traffic measured 41% of
// consecutive chat requests from one caller switching model within 30 minutes,
// while only 3.5% exceeded the 5-minute cache TTL — the switching, not expiry,
// is what was throwing the cache away.
//
// The fix is deliberately the smallest one that can work: after the router has
// chosen, if this conversation's previous model is STILL IN THE POOL the router
// chose from, pick that instead.
//
// Everything this must not break is already enforced by the pool. By the time
// the router sees candidates they have been filtered for the key's allowlist,
// the capabilities this request needs, the modalities it actually carries, and
// a context window that holds the estimated prompt. So:
//
//   - Model polymorphism: a conversation that adds an image reaches a request
//     whose modality filter drops every text-only model. If the remembered model
//     is one of them it is not in the pool, and affinity stands down.
//   - Context growth: filterByContextWindow uses the CONSERVATIVE token estimate
//     precisely because under-counting hard-400s upstream. A conversation that
//     outgrows the remembered model's window drops it from the pool, and affinity
//     stands down.
//
// Affinity only ever REORDERS INSIDE the pool. It never adds a candidate, never
// relaxes a filter, and never skips the router call — skipping the call would
// also skip building the pool, which is the very thing that proves the
// remembered model is still allowed to serve this request.
//
// Only chat traffic participates. Embeddings have no prompt-cache prefix to
// preserve, and the other endpoint kinds do not route through this strategy's
// conversational pool at all.

// affinityTTL is how long a remembered model stays authoritative.
//
// It tracks the PROVIDER'S cache lifetime, not the conversation's. Anthropic's
// default ephemeral entry lives five minutes, measured from the start of the
// request that wrote it. Once it has expired there is no cache left to protect
// and no reason to keep overriding the router's judgement — a conversation
// resumed an hour later should get the model the router thinks is right today.
const affinityTTL = 5 * time.Minute

// SessionAffinityStore remembers, per (virtual key, session), the model a
// conversation last used. Implementations are expected to be a short-TTL cache;
// losing an entry costs one cache miss, never correctness, so every method is
// allowed to fail silently.
type SessionAffinityStore interface {
	// Get returns the remembered model id for this conversation, or "" when
	// there is none. Errors are indistinguishable from absence on purpose.
	Get(ctx context.Context, vkID, sessionID string) string
	// Put records the model this conversation just used, with affinityTTL.
	Put(ctx context.Context, vkID, sessionID, modelID string)
}

// maxSessionIDBytes caps the tag the same way the audit path caps it. An
// uncapped caller-supplied string would become an uncapped cache key.
const maxSessionIDBytes = 256

// affinityKey is the ONE gate. It returns the (virtual key, session) pair this
// request participates under, and ok=false for every request that must not
// participate at all.
//
// Every caller goes through it, and nothing else in this file reads the header
// or the virtual key. That is deliberate: the previous shape checked "is the
// session id empty" separately in each function, which works until someone adds
// a third call site and forgets — and the failure mode of forgetting is that
// every request WITHOUT a session id shares one empty-string key and gets pinned
// to whatever model the last unrelated conversation happened to use. There is no
// key to pass to the store unless this returns ok.
//
// It refuses, in order:
//   - no virtual key: the key would not be scoped to a caller
//   - not a chat request: an embedding has no prompt-cache prefix to keep warm
//   - no session id, or one that is only whitespace: the caller did not tell us
//     which conversation this is, and guessing is worse than not participating
//
// The session id is caller-asserted and never validated — the same trust model
// the header already carries as an audit tag. A caller that sends the wrong id
// pins its own conversations together, costing itself cache hits; the key is
// scoped by virtual key, so its tag can never reach another caller's traffic.
func affinityKey(rctx *core.RoutingContext) (vkID, sessionID string, ok bool) {
	if rctx == nil || rctx.VirtualKey == nil || rctx.VirtualKey.ID == "" {
		return "", "", false
	}
	if rctx.EndpointType != typology.EndpointKindChat {
		return "", "", false
	}
	sessionID = strings.TrimSpace(rctx.Headers.Get("X-Nexus-Session-Id"))
	if sessionID == "" {
		return "", "", false
	}
	if len(sessionID) > maxSessionIDBytes {
		sessionID = sessionID[:maxSessionIDBytes]
	}
	return rctx.VirtualKey.ID, sessionID, true
}

// applySessionAffinity returns the model id to dispatch: the remembered one when
// this conversation has a live entry that is still in the pool, otherwise the
// router's own pick unchanged. The second return says whether the override fired,
// so the caller can record it in the routing trace — a routing decision nobody
// can see afterwards is one nobody can evaluate.
func applySessionAffinity(
	ctx context.Context,
	store SessionAffinityStore,
	rctx *core.RoutingContext,
	candidates []core.SmartModelRow,
	routerPick string,
) (string, bool) {
	if store == nil {
		return routerPick, false
	}
	vkID, sessionID, ok := affinityKey(rctx)
	if !ok {
		return routerPick, false
	}
	remembered := store.Get(ctx, vkID, sessionID)
	if remembered == "" || remembered == routerPick {
		return routerPick, false
	}
	// The pool is the authority on what may serve this request. Membership is
	// the whole check — it already carries modality, context window, capability
	// and allowlist.
	for i := range candidates {
		if candidates[i].ModelID == remembered {
			return remembered, true
		}
	}
	return routerPick, false
}

// rememberSessionModel records the dispatched model for the next turn. Called
// with whatever was finally selected, affinity or not, so a conversation that
// legitimately moved model (its previous one left the pool) starts tracking the
// new one instead of retrying a dead entry every turn.
func rememberSessionModel(ctx context.Context, store SessionAffinityStore, rctx *core.RoutingContext, modelID string) {
	if store == nil || modelID == "" {
		return
	}
	vkID, sessionID, ok := affinityKey(rctx)
	if !ok {
		return
	}
	store.Put(ctx, vkID, sessionID, modelID)
}
