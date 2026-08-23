package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// The quota downgrade re-picks which model serves the request. Everything here
// asserts on the model the UPSTREAM was actually asked for — the request body's
// `model` field — because that is the only statement about the downgrade that a
// caller can feel. Asserting on the x-nexus-quota-downgrade header proves a
// header was set and says nothing about which model ran; that is the shape of
// assertion this program has already been burned by.

// modelRecordingUpstream answers as whichever model the request names, records
// every model it was asked for in order, and fails the models named in `fail`
// with 503 so a failover can be observed.
type modelRecordingUpstream struct {
	mu   sync.Mutex
	seen []string
	fail map[string]bool
}

func (u *modelRecordingUpstream) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &req)

	u.mu.Lock()
	u.seen = append(u.seen, req.Model)
	shouldFail := u.fail[req.Model]
	u.mu.Unlock()

	if shouldFail {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream down"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"` + req.Model + `",
		"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
}

func (u *modelRecordingUpstream) models() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.seen...)
}

// first returns the model the FIRST dispatch named — the one the downgrade
// selected. A later failover appends to the same list, so "which model did the
// downgrade pick" is specifically seen[0], not "seen contains".
func (u *modelRecordingUpstream) first(t *testing.T) string {
	t.Helper()
	got := u.models()
	if len(got) == 0 {
		t.Fatal("upstream was never dispatched; the downgrade rejected instead of selecting")
	}
	return got[0]
}

// stubPricingByMap mirrors what BOTH production implementations of
// FetchModelPricing guarantee (store.DB and cache layer.Layer): exactly one row
// per requested ID, in the requested order, with a zero row for an ID it has no
// price for. The downgrade selector indexes the returned slice and then indexes
// its own candidate slice with the same number, so a stub that returned rows in
// a different order or dropped one would hide the very correspondence the tests
// below exist to pin down.
type stubPricingByMap struct {
	stubModels
	prices map[string]store.ModelPricing
}

func (s *stubPricingByMap) FetchModelPricing(_ context.Context, ids []string) ([]store.ModelPricing, error) {
	out := make([]store.ModelPricing, len(ids))
	for i, id := range ids {
		mp := s.prices[id]
		mp.ModelID = id
		out[i] = mp
	}
	return out, nil
}

func target(modelID string, upgradeOnly bool) routingcore.RoutingTarget {
	return routingcore.RoutingTarget{
		ProviderID:         "p-openai",
		ProviderName:       "openai",
		ProviderModelID:    modelID,
		ModelID:            modelID,
		ModelName:          modelID,
		ModelCode:          modelID,
		AdapterType:        "openai",
		ContextUpgradeOnly: upgradeOnly,
	}
}

// downgradeDeps wires a quota policy that forces a downgrade ($5 limit, $1
// used → $4 of headroom) over the supplied targets and price map. The primary
// is priced astronomically so its estimate blows the cap; that is what makes
// the engine answer "downgrade" rather than "allow".
func downgradeDeps(t *testing.T, upstreamURL string, targets []routingcore.RoutingTarget,
	prices map[string]store.ModelPricing) *Deps {
	t.Helper()
	primary := &store.Model{ID: targets[0].ModelID, InputPricePM: fPtr(1_000_000.0), OutputPricePM: fPtr(1_000_000.0)}
	byID := map[string]*store.Model{}
	byCode := map[string]*store.Model{}
	for _, tg := range targets {
		m := primary
		if tg.ModelID != targets[0].ModelID {
			m = &store.Model{ID: tg.ModelID, InputPricePM: fPtr(0.01), OutputPricePM: fPtr(0.01)}
		}
		byID[tg.ModelID] = m
		byCode[tg.ModelID] = m
	}
	return makeOpenAIDeps(t, upstreamURL, emptyHookCache(t), func(d *Deps) {
		d.QuotaEngine = newQuotaEngineWithPolicy(t, "virtual_key", "downgrade", 500, 100)
		d.Router = &stubRouterCacheTest{targets: targets}
		d.Models = &stubPricingByMap{
			stubModels: stubModels{byID: byID, byCode: byCode},
			prices:     prices,
		}
	})
}

func postChat(t *testing.T, deps *Deps, model string) *httptest.ResponseRecorder {
	t.Helper()
	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"` + model + `","max_tokens":100,"messages":[{"role":"user","content":"x"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

// An armed context-upgrade target is in the list for one reason: to take over
// when the context overflows. It was never selected for its ability to serve
// THIS request, and the executor will skip it for exactly that reason. Letting
// the price comparison promote it to primary makes it serve a request nothing
// selected it for — and it wins on price often, because a large-window model
// parked as an upgrade path is frequently a cheaper tier than the primary.
//
// The price map here is arranged so the upgrade target is strictly the cheapest
// candidate: if it is in the candidate pool at all, it wins. So this is also
// what pins down the indexing. The selector returns an index into the slice it
// was handed, and the caller must resolve it against the SAME slice — filter
// the pool but index the unfiltered list and this test dispatches the upgrade
// target just as surely as not filtering at all.
func TestQuotaDowngrade_ArmedUpgradeTargetIsNeverThePickedModel(t *testing.T) {
	up := &modelRecordingUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(up.serve))
	defer srv.Close()

	targets := []routingcore.RoutingTarget{
		target("expensive-primary", false),
		target("ctx-upgrade", true), // armed for overflow only, and the cheapest thing here
		target("cheap-ordinary", false),
	}
	prices := map[string]store.ModelPricing{
		"expensive-primary": {InputPricePM: 1_000_000, OutputPricePM: 1_000_000, Priced: true},
		"ctx-upgrade":       {InputPricePM: 0.001, OutputPricePM: 0.001, Priced: true},
		"cheap-ordinary":    {InputPricePM: 0.01, OutputPricePM: 0.01, Priced: true},
	}

	w := postChat(t, downgradeDeps(t, srv.URL, targets, prices), "expensive-primary")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	if got := up.first(t); got != "cheap-ordinary" {
		t.Errorf("downgrade dispatched %q; want cheap-ordinary — the cheapest ORDINARY target.\n"+
			"  ctx-upgrade is cheaper but is armed for context overflow only.\n"+
			"  models dispatched, in order: %v", got, up.models())
	}
}

// The downgrade rewrites the target list. Replacing it with just the selected
// model leaves the request with no recovery at all: a transient failure that
// the original list would have absorbed becomes terminal, so being near a
// quota costs the caller their failover as well as their model — and nothing
// in the response says so.
func TestQuotaDowngrade_KeepsTheRemainingTargetsAsFallbacks(t *testing.T) {
	up := &modelRecordingUpstream{fail: map[string]bool{"cheap-ordinary": true}}
	srv := httptest.NewServer(http.HandlerFunc(up.serve))
	defer srv.Close()

	targets := []routingcore.RoutingTarget{
		target("expensive-primary", false),
		target("cheap-ordinary", false),
		target("cheap-backup", false),
	}
	prices := map[string]store.ModelPricing{
		"expensive-primary": {InputPricePM: 1_000_000, OutputPricePM: 1_000_000, Priced: true},
		"cheap-ordinary":    {InputPricePM: 0.01, OutputPricePM: 0.01, Priced: true},
		"cheap-backup":      {InputPricePM: 0.02, OutputPricePM: 0.02, Priced: true},
	}

	w := postChat(t, downgradeDeps(t, srv.URL, targets, prices), "expensive-primary")

	if got := up.first(t); got != "cheap-ordinary" {
		t.Fatalf("downgrade dispatched %q first; want cheap-ordinary", got)
	}
	seen := up.models()
	distinct := map[string]bool{}
	for _, m := range seen {
		distinct[m] = true
	}
	if len(distinct) < 2 {
		t.Fatalf("the downgraded request never failed over: dispatched only %v.\n"+
			"  cheap-ordinary answered 503; the other targets must remain as fallbacks", seen)
	}
	if w.Code != http.StatusOK {
		t.Errorf("status=%d want 200 — a fallback target was available and answers 200; body=%s",
			w.Code, w.Body.String())
	}
}

// The unpriced guard has to survive the reordering too: a candidate with no
// price row prices to $0, would win the cheapest-fits comparison outright, and
// then slips past the very cost cap that triggered the downgrade. With the only
// alternative unpriced there is no affordable model, and the request must be
// refused rather than served on an uncountable one.
func TestQuotaDowngrade_UnpricedCandidateIsNotAnEscapeHatch(t *testing.T) {
	up := &modelRecordingUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(up.serve))
	defer srv.Close()

	targets := []routingcore.RoutingTarget{
		target("expensive-primary", false),
		target("unpriced-model", false),
	}
	prices := map[string]store.ModelPricing{
		"expensive-primary": {InputPricePM: 1_000_000, OutputPricePM: 1_000_000, Priced: true},
		"unpriced-model":    {Priced: false},
	}

	w := postChat(t, downgradeDeps(t, srv.URL, targets, prices), "expensive-primary")
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status=%d want 429; body=%s", w.Code, w.Body.String())
	}
	if got := up.models(); len(got) != 0 {
		t.Errorf("dispatched %v; an unpriced model must never serve a downgrade", got)
	}
}

// TestQuotaDowngrade_ReachesIntoTheRuleChain pins a property that currently
// holds by accident and would be lost by a change that looks like tidying.
//
// The downgrade picks the cheapest target a near-cap key can still afford. It
// sees every target because the plan is one flat list — the strategy's answer
// and the rule's own chain concatenated. That is the arrangement the rebuild
// replaces with two named lists, and a split that hands the downgrade only the
// strategy's half would take the cheap models away from precisely the requests
// that need them: the rule's chain is where an admin puts the cheaper backup.
//
// The failure would not look like a bug. The request returns "no affordable
// model" — a truthful-sounding refusal about a model that was right there.
//
// Written before the split so the split cannot pass without preserving it.
func TestQuotaDowngrade_ReachesIntoTheRuleChain(t *testing.T) {
	up := &modelRecordingUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(up.serve))
	defer srv.Close()

	// The strategy's answer is expensive; the only affordable model is the one
	// the rule carries as its chain.
	strategyPick := target("expensive-primary", false)
	ruleChain := target("cheap-from-the-chain", false)
	ruleChain.Source = "fallback"

	prices := map[string]store.ModelPricing{
		"expensive-primary":    {InputPricePM: 1_000_000, OutputPricePM: 1_000_000, Priced: true},
		"cheap-from-the-chain": {InputPricePM: 0.01, OutputPricePM: 0.01, Priced: true},
	}

	w := postChat(t, downgradeDeps(t, srv.URL,
		[]routingcore.RoutingTarget{strategyPick, ruleChain}, prices), "expensive-primary")

	if got := up.first(t); got != "cheap-from-the-chain" {
		t.Fatalf("downgrade dispatched %q; the only affordable model was in the rule's own "+
			"chain, and a plan that hides it from the downgrade refuses the request over a "+
			"model that was available", got)
	}
	if w.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
}
