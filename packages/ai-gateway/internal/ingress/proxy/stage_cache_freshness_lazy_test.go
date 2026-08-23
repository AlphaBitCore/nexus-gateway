package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	cachecore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/freshness"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// freshnessMessages decides whether the request pays for a full canonical
// Normalize. The observable cost is the normalizer call count, so every case
// below asserts on that rather than on the returned slice alone — a test that
// only checked the slice was nil would pass even if the canonical had been
// computed and thrown away, which is the exact defect being fixed.
//
// Shipped default posture: cache.enabled is true out of the box,
// apply_freshness_rules is absent from ai-gateway.config.yaml (so false), and
// FreshnessDetector stays nil until a pattern config is pushed. In that
// posture the detector arm of classifyCachePreLookup cannot fire, so
// computing its input is pure waste on every chat request.

// newLazyFixture mirrors newCanonicalFixture but turns lazyCanonical ON
// before the request context is built. That flag is what decides whether
// buildRequestContext installs a lazy seam or computes the canonical eagerly
// at admission — with it off, the canonical is already memoized by the time
// any test runs, and a call-count assertion passes no matter what the code
// under test does.
func newLazyFixture(t *testing.T, stub *bodyEchoNormalize, body []byte, applyRules bool, detector *freshness.Detector) (*proxyState, *bodyEchoNormalize) {
	t.Helper()
	reg := normalize.NewRegistry()
	reg.Register("openai", stub)

	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mini.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	h := &Handler{deps: &Deps{
		NormalizeRegistry: reg,
		FreshnessDetector: detector,
		Cache: cachecore.New(rdb, cachecore.Config{
			Enabled:             true,
			ApplyFreshnessRules: applyRules,
		}, slog.New(slog.NewTextHandler(io.Discard, nil))),
	}}
	h.lazyCanonical = true

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rctx := h.buildRequestContext(req, nil, body, provcore.FormatOpenAI, "gpt-4o", "chat")

	s := &proxyState{
		h:        h,
		r:        req,
		rec:      &audit.Record{},
		resolved: Ingress{BodyFormat: provcore.FormatOpenAI},
		rctxFull: rctx,
		body:     body,
		modelID:  "gpt-4o",
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if stub.calls != 0 {
		t.Fatalf("fixture normalized %d time(s) at admission; the canonical must still be lazy or every assertion below is vacuous", stub.calls)
	}
	return s, stub
}

func TestFreshnessMessages_ShippedDefault_DoesNotMaterializeCanonical(t *testing.T) {
	s, stub := newLazyFixture(t, &bodyEchoNormalize{id: "openai"}, []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`), false, nil)
	before := stub.calls

	if msgs := s.freshnessMessages(true); msgs != nil {
		t.Errorf("messages=%v, want nil when the detector cannot act on them", msgs)
	}
	if stub.calls != before {
		t.Errorf("normalizer ran %d extra time(s); the canonical must not be computed for a predicate that cannot fire",
			stub.calls-before)
	}
}

// apply_freshness_rules on but no detector pushed yet is the window between
// an operator flipping the flag and the pattern config arriving. The arm
// still cannot fire, so the cost is still not owed.
func TestFreshnessMessages_RulesOnButNoDetector_DoesNotMaterializeCanonical(t *testing.T) {
	s, stub := newLazyFixture(t, &bodyEchoNormalize{id: "openai"}, []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`), true, nil)
	before := stub.calls

	if msgs := s.freshnessMessages(true); msgs != nil {
		t.Errorf("messages=%v, want nil while no detector is loaded", msgs)
	}
	if stub.calls != before {
		t.Errorf("normalizer ran %d extra time(s) with no detector loaded", stub.calls-before)
	}
}

// A detector loaded but the rules flag off is the reverse window, and the
// consumer's guard is an AND — so this must also decline.
func TestFreshnessMessages_DetectorButRulesOff_DoesNotMaterializeCanonical(t *testing.T) {
	s, stub := newLazyFixture(t, &bodyEchoNormalize{id: "openai"}, []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`), false, newTestDetector(t))
	before := stub.calls

	if msgs := s.freshnessMessages(true); msgs != nil {
		t.Errorf("messages=%v, want nil while apply_freshness_rules is off", msgs)
	}
	if stub.calls != before {
		t.Errorf("normalizer ran %d extra time(s) with the rules flag off", stub.calls-before)
	}
}

// Cache off short-circuits ahead of everything else — no tier, no lookup, no
// reason to hold a canonical.
func TestFreshnessMessages_CacheOff_DoesNotMaterializeCanonical(t *testing.T) {
	s, stub := newLazyFixture(t, &bodyEchoNormalize{id: "openai"}, []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`), true, newTestDetector(t))
	before := stub.calls

	if msgs := s.freshnessMessages(false); msgs != nil {
		t.Errorf("messages=%v, want nil with every cache tier off", msgs)
	}
	if stub.calls != before {
		t.Errorf("normalizer ran %d extra time(s) with cache off", stub.calls-before)
	}
}

// The other half of the contract: when the detector CAN fire, the messages
// must actually arrive. A guard that never materializes is not a fix, it is
// a silently disabled feature.
func TestFreshnessMessages_DetectorArmed_DeliversCanonicalMessages(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"what is the weather today"}]}`)
	s, stub := newLazyFixture(t, &bodyEchoNormalize{id: "openai"}, body, true, newTestDetector(t))

	msgs := s.freshnessMessages(true)
	if stub.calls != 1 {
		t.Fatalf("normalizer ran %d time(s); the armed path must materialize the canonical exactly once", stub.calls)
	}
	if len(msgs) == 0 {
		t.Fatal("no messages delivered while the detector is armed — the freshness rule can never match")
	}
	if msgs[0].Content != string(body) {
		t.Errorf("content=%q, want the canonical text %q", msgs[0].Content, string(body))
	}
}

// newTestDetector builds a detector with no rules. Rule content is
// irrelevant here — what matters is only whether a detector is present, which
// is the condition classifyCachePreLookup's arm actually tests.
func newTestDetector(t *testing.T) *freshness.Detector {
	t.Helper()
	d, err := freshness.NewDetector(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "test", prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("NewDetector: %v", err)
	}
	return d
}
