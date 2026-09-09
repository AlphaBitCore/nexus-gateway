package strategies

import (
	"context"
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// recordingStore fails the test if it is touched at all, so "affinity stood
// down" can be asserted as "the store was never reached" rather than as "the
// answer happened to come out the same".
type recordingStore struct {
	t       *testing.T
	gets    []string
	puts    []string
	stored  map[string]string
	forbid  bool
	forbidW string
}

func (s *recordingStore) key(vk, sess string) string { return vk + "|" + sess }

func (s *recordingStore) Get(_ context.Context, vk, sess string) string {
	if s.forbid {
		s.t.Fatalf("%s: the store must not be consulted at all; got Get(vk=%q, session=%q)", s.forbidW, vk, sess)
	}
	s.gets = append(s.gets, s.key(vk, sess))
	return s.stored[s.key(vk, sess)]
}

func (s *recordingStore) Put(_ context.Context, vk, sess, model string) {
	if s.forbid {
		s.t.Fatalf("%s: the store must not be written at all; got Put(vk=%q, session=%q, model=%q)", s.forbidW, vk, sess, model)
	}
	if s.stored == nil {
		s.stored = map[string]string{}
	}
	s.stored[s.key(vk, sess)] = model
	s.puts = append(s.puts, s.key(vk, sess))
}

func rctxWith(sessionHeader string, endpoint typology.EndpointKind, vkID string) *core.RoutingContext {
	h := http.Header{}
	if sessionHeader != "" {
		h.Set("X-Nexus-Session-Id", sessionHeader)
	}
	rc := &core.RoutingContext{EndpointType: endpoint, Headers: core.NewSafeHeaders(h)}
	if vkID != "" {
		rc.VirtualKey = &core.VKContext{ID: vkID}
	}
	return rc
}

func pool(ids ...string) []core.SmartModelRow {
	out := make([]core.SmartModelRow, 0, len(ids))
	for _, id := range ids {
		out = append(out, core.SmartModelRow{ModelID: id, ProviderID: "p"})
	}
	return out
}

// The failure this gate exists to prevent: a request with NO session id must not
// reach the store. If it did, every such request would share one empty-string
// key and get pinned to whatever model the last unrelated conversation used —
// affinity between strangers.
func TestSessionAffinity_NeverTouchesTheStoreWithoutASessionID(t *testing.T) {
	for _, tc := range []struct {
		name     string
		header   string
		endpoint typology.EndpointKind
		vkID     string
	}{
		{"header absent", "", typology.EndpointKindChat, "vk-1"},
		{"header present but empty", "   ", typology.EndpointKindChat, "vk-1"},
		{"header is a tab", "\t", typology.EndpointKindChat, "vk-1"},
		{"no virtual key", "sess-1", typology.EndpointKindChat, ""},
		{"embeddings, not chat", "sess-1", typology.EndpointKindEmbeddings, "vk-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &recordingStore{t: t, forbid: true, forbidW: tc.name}
			rctx := rctxWith(tc.header, tc.endpoint, tc.vkID)

			got, overridden := applySessionAffinity(context.Background(), store, rctx, pool("a", "b"), "a")
			if overridden || got != "a" {
				t.Fatalf("%s: must return the router pick untouched, got %q (overridden=%v)", tc.name, got, overridden)
			}
			rememberSessionModel(context.Background(), store, rctx, "a")
		})
	}
}

// The happy path, and the reason the feature exists.
func TestSessionAffinity_KeepsTheConversationOnItsModel(t *testing.T) {
	store := &recordingStore{t: t}
	rctx := rctxWith("sess-1", typology.EndpointKindChat, "vk-1")

	// Turn 1: nothing remembered, the router's pick stands and is recorded.
	got, overridden := applySessionAffinity(context.Background(), store, rctx, pool("claude-sonnet-4-6", "gpt-4o"), "claude-sonnet-4-6")
	if overridden || got != "claude-sonnet-4-6" {
		t.Fatalf("turn 1 must take the router pick, got %q (overridden=%v)", got, overridden)
	}
	rememberSessionModel(context.Background(), store, rctx, got)

	// Turn 2: the router now prefers a different model, but the conversation's
	// prefix is cached on the first one and it is still in the pool.
	got, overridden = applySessionAffinity(context.Background(), store, rctx, pool("claude-sonnet-4-6", "gpt-4o"), "gpt-4o")
	if !overridden || got != "claude-sonnet-4-6" {
		t.Fatalf("turn 2 must keep the conversation on its cached model, got %q (overridden=%v)", got, overridden)
	}
}

// The pool is the authority. A remembered model that the request's own filters
// dropped — it lost the modality it needs, or the conversation outgrew its
// context window — is simply not in the pool, and affinity must stand down
// rather than resurrect it.
func TestSessionAffinity_StandsDownWhenTheModelLeftThePool(t *testing.T) {
	store := &recordingStore{t: t}
	rctx := rctxWith("sess-1", typology.EndpointKindChat, "vk-1")
	rememberSessionModel(context.Background(), store, rctx, "text-only-model")

	// This turn carries an image, so the modality filter left only vision models.
	got, overridden := applySessionAffinity(context.Background(), store, rctx, pool("vision-model", "other-vision"), "vision-model")
	if overridden || got != "vision-model" {
		t.Fatalf("a model outside the pool must never be selected, got %q (overridden=%v)", got, overridden)
	}

	// And the new pick replaces the dead entry, so the next turn does not keep
	// re-testing a model that already lost.
	rememberSessionModel(context.Background(), store, rctx, got)
	if got, _ := applySessionAffinity(context.Background(), store, rctx, pool("vision-model", "other-vision"), "other-vision"); got != "vision-model" {
		t.Fatalf("the newly dispatched model must become the remembered one, got %q", got)
	}
}

// One caller's tag can never reach another's traffic: the key is scoped by
// virtual key, so two callers using the same session string stay independent.
func TestSessionAffinity_KeysAreScopedByVirtualKey(t *testing.T) {
	store := &recordingStore{t: t}
	a := rctxWith("shared-session-name", typology.EndpointKindChat, "vk-A")
	b := rctxWith("shared-session-name", typology.EndpointKindChat, "vk-B")

	rememberSessionModel(context.Background(), store, a, "model-A")
	got, overridden := applySessionAffinity(context.Background(), store, b, pool("model-A", "model-B"), "model-B")
	if overridden || got != "model-B" {
		t.Fatalf("vk-B must not inherit vk-A's affinity, got %q (overridden=%v)", got, overridden)
	}
}

// A nil store is the disabled state, and must be inert on every path.
func TestSessionAffinity_NilStoreIsInert(t *testing.T) {
	rctx := rctxWith("sess-1", typology.EndpointKindChat, "vk-1")
	got, overridden := applySessionAffinity(context.Background(), nil, rctx, pool("a"), "a")
	if overridden || got != "a" {
		t.Fatalf("a nil store must change nothing, got %q (overridden=%v)", got, overridden)
	}
	rememberSessionModel(context.Background(), nil, rctx, "a") // must not panic
}

// An oversized tag is capped rather than becoming an unbounded cache key.
func TestSessionAffinity_SessionIDIsCapped(t *testing.T) {
	long := make([]byte, maxSessionIDBytes+50)
	for i := range long {
		long[i] = 'x'
	}
	_, sess, ok := affinityKey(rctxWith(string(long), typology.EndpointKindChat, "vk-1"))
	if !ok {
		t.Fatal("a long but valid session id must still participate")
	}
	if len(sess) != maxSessionIDBytes {
		t.Fatalf("session id must be capped at %d bytes, got %d", maxSessionIDBytes, len(sess))
	}
}
