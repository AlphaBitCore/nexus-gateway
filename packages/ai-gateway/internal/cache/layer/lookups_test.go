package cachelayer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
)

// primeSnapshots loads a small fixed dataset into the snapshot caches
// so the Get* lookups have something to read.
func primeSnapshots(t *testing.T, mock pgxmock.PgxPoolIface, l *Layer) {
	t.Helper()
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`FROM "Provider"`).
		WillReturnRows(pgxmock.NewRows(providerCols).
			AddRow("p1", "openai", strPtr("OpenAI"), "openai",
				"https://api.openai.com", "/v1", nil, nil, true, (*bool)(nil)).
			AddRow("p2", "anthropic", strPtr("Anthropic"), "anthropic",
				"https://api.anthropic.com", "/v1", nil, nil, true, (*bool)(nil)))
	mock.ExpectQuery(`FROM "Model" m`).
		WillReturnRows(pgxmock.NewRows(modelCols).
			AddRow(makeModelRow("m1", "gpt-4o", "p1", true)...).
			// alias-only row: code != "claude-bridge" but aliases include it
			AddRow(makeModelRowWithAliases("m2", "claude-3", "p2", true, []string{"claude-bridge"})...).
			// disabled row should be excluded from code/index but included in byID
			AddRow(makeModelRow("m3", "disabled-model", "p1", false)...))
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows(credentialCols).
			AddRow(makeCredRow("c1", "p1", true, "active")...).
			AddRow(makeCredRow("c2", "p1", true, "retiring")...). // not active → excluded from byProvider/list
			AddRow(makeCredRow("c3", "p2", false, "active")...)). // not enabled → excluded
		RowsWillBeClosed()
	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

func makeModelRowWithAliases(id, code, providerID string, enabled bool, aliases []string) []any {
	r := makeModelRow(id, code, providerID, enabled)
	r[modelColIdx("aliases")] = aliases
	return r
}

func TestGetProvider_HitAndMiss(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	primeSnapshots(t, mock, l)

	got, err := l.GetProvider(context.Background(), "p1")
	if err != nil || got == nil || got.Name != "openai" {
		t.Fatalf("hit: got %+v err=%v", got, err)
	}

	_, err = l.GetProvider(context.Background(), "missing")
	if !IsNotFound(err) || !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("miss must wrap pgx.ErrNoRows; got %v", err)
	}
	if !strings.Contains(err.Error(), `provider "missing"`) {
		t.Errorf("miss msg should include id: %v", err)
	}
}

func TestGetModel_HitAndMiss(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	primeSnapshots(t, mock, l)

	got, err := l.GetModel(context.Background(), "m1")
	if err != nil || got == nil || got.Code != "gpt-4o" {
		t.Fatalf("hit: got %+v err=%v", got, err)
	}
	if _, err := l.GetModel(context.Background(), "missing"); !IsNotFound(err) {
		t.Errorf("miss must report not-found; got %v", err)
	}
}

func TestGetModelByCode_HitAndMissAndDisabled(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	primeSnapshots(t, mock, l)

	if got, err := l.GetModelByCode(context.Background(), "gpt-4o"); err != nil || got.ID != "m1" {
		t.Fatalf("enabled code lookup failed: %+v %v", got, err)
	}
	// Disabled rows are absent from the by-code index.
	if _, err := l.GetModelByCode(context.Background(), "disabled-model"); !IsNotFound(err) {
		t.Errorf("disabled code must miss; got %v", err)
	}
	if _, err := l.GetModelByCode(context.Background(), "nope"); !IsNotFound(err) {
		t.Errorf("absent code must miss; got %v", err)
	}
}

func TestGetModelByCode_NilIndex(t *testing.T) {
	// Brand-new layer with no snapshot loaded: byCode index is nil.
	//
	// This must NOT read as "gpt-4o does not exist". The model was never
	// looked up — there was no index to look in — and a caller that turns a
	// miss into 404 would tell the client a permanent lie about a condition
	// that clears as soon as the catalog loads.
	mock, l := newMockLayer(t, Config{})
	_ = mock
	_, err := l.GetModelByCode(context.Background(), "gpt-4o")
	if !IsIndexUnavailable(err) {
		t.Errorf("nil index must report index-unavailable; got %v", err)
	}
	if IsNotFound(err) {
		t.Errorf("an unloaded index must not masquerade as a missing row; got %v", err)
	}
}

// TestGetModelByCode_LoadedIndexMissIsNotFound is the other half: once the
// index exists, a key that is not in it really is absent, and must stay a
// plain not-found so the 404 path still works.
func TestGetModelByCode_LoadedIndexMissIsNotFound(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	primeSnapshots(t, mock, l)
	_, err := l.GetModelByCode(context.Background(), "definitely-not-a-model")
	if !IsNotFound(err) {
		t.Errorf("a miss against a loaded index must be not-found; got %v", err)
	}
	if IsIndexUnavailable(err) {
		t.Errorf("a genuine miss must not be reported as an unloaded index; got %v", err)
	}
}

func TestResolveModelCandidates_MatchesCodeOrAlias(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	primeSnapshots(t, mock, l)

	// Empty code returns nil (fast path).
	if got, err := l.ResolveModelCandidates(context.Background(), ""); err != nil || got != nil {
		t.Errorf("empty code: want (nil,nil); got %v %v", got, err)
	}
	// Direct code match
	got, err := l.ResolveModelCandidates(context.Background(), "gpt-4o")
	if err != nil || len(got) != 1 || got[0].ID != "m1" {
		t.Errorf("code match: got %+v err=%v", got, err)
	}
	// Alias-only match
	got, err = l.ResolveModelCandidates(context.Background(), "claude-bridge")
	if err != nil || len(got) != 1 || got[0].ID != "m2" {
		t.Errorf("alias match: got %+v err=%v", got, err)
	}
	// Disabled rows excluded.
	got, err = l.ResolveModelCandidates(context.Background(), "disabled-model")
	if err != nil || len(got) != 0 {
		t.Errorf("disabled excluded: got %+v", got)
	}
}

func TestListEnabledModels_ExcludesDisabledAndOrders(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	primeSnapshots(t, mock, l)

	out, err := l.ListEnabledModels(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2 (disabled excluded)", len(out))
	}
	// Sort key: providerID ASC, then Name ASC. m1 (p1) before m2 (p2).
	if out[0].ProviderID != "p1" || out[1].ProviderID != "p2" {
		t.Errorf("unexpected order: %+v", out)
	}
}

// TestProviderDisabled_ModelIsUnservableButStillPriceable pins the whole
// contract of a model whose row is enabled while its provider is switched off:
// it disappears from every servable surface (catalog list, strict code lookup,
// code-or-alias lookup used by the passthrough route) yet stays readable by
// UUID for pricing/metering and stays resolvable as a routing CANDIDATE, so a
// rule that redirects traffic off the disabled provider can still match it.
//
// A catalog that advertises what the router refuses is worse than one that is
// simply smaller: the client picks the advertised model and the call dies
// upstream instead of never being offered.
func TestProviderDisabled_ModelIsUnservableButStillPriceable(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`FROM "Provider"`).
		WillReturnRows(pgxmock.NewRows(providerCols).
			AddRow("p-on", "openai", strPtr("OpenAI"), "openai",
				"https://api.openai.com", "/v1", nil, nil, true, (*bool)(nil)).
			AddRow("p-off", "cohere", strPtr("Cohere"), "cohere",
				"https://api.cohere.com", "/v2", nil, nil, false, (*bool)(nil)))
	live := makeModelRow("m-live", "gpt-4o", "p-on", true)
	// Enabled model on a DISABLED provider, addressable by code and by alias.
	dead := makeModelRowOnProvider("m-dead", "embed-english-v3.0", "p-off", true, false)
	dead[modelColIdx("aliases")] = []string{"embed-en"}
	mock.ExpectQuery(`FROM "Model" m`).
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(live...).AddRow(dead...))
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows(credentialCols))
	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx := context.Background()

	out, err := l.ListEnabledModels(ctx)
	if err != nil {
		t.Fatalf("ListEnabledModels: %v", err)
	}
	if len(out) != 1 || out[0].ID != "m-live" {
		t.Fatalf("catalog = %+v, want only m-live (provider-disabled model must not be advertised)", out)
	}

	if _, err := l.GetModelByCode(ctx, "embed-english-v3.0"); !IsNotFound(err) {
		t.Errorf("GetModelByCode on a disabled provider must miss; got %v", err)
	}
	for _, key := range []string{"embed-english-v3.0", "embed-en"} {
		if _, err := l.GetModelByCodeOrAlias(ctx, key); !IsNotFound(err) {
			t.Errorf("GetModelByCodeOrAlias(%q) must miss so the passthrough route cannot dispatch to a disabled provider; got %v", key, err)
		}
	}

	// Pricing/metering reads by UUID must still resolve: a request that was
	// already in flight when the provider was switched off still has to be
	// costed, and quota downgrade prices candidates it will never call.
	got, err := l.GetModel(ctx, "m-dead")
	if err != nil || got == nil {
		t.Fatalf("GetModel by UUID must still resolve for pricing; got %+v err=%v", got, err)
	}
	if got.Enabled != true || got.ProviderEnabled != false {
		t.Errorf("row must carry both flags verbatim; got Enabled=%v ProviderEnabled=%v", got.Enabled, got.ProviderEnabled)
	}
	if got.Servable() {
		t.Error("Servable() must be false for an enabled model on a disabled provider")
	}

	// Routing candidates keep the row: a rule redirecting this model to another
	// provider matches on its UUID and must still fire.
	cands, err := l.ResolveModelCandidates(ctx, "embed-english-v3.0")
	if err != nil || len(cands) != 1 || cands[0].ID != "m-dead" {
		t.Errorf("ResolveModelCandidates must keep the requested model resolvable; got %+v err=%v", cands, err)
	}
}

// TestStatusDisabled_WithdrawsModelFromService covers the second switch an
// operator can flip. `disabled` is the only status that withdraws a model:
// `deprecated` and `preview` rows stay callable and merely carry a different
// label, so a predicate written as status = 'active' would silently take
// preview models out of service.
func TestStatusDisabled_WithdrawsModelFromService(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`FROM "Provider"`).
		WillReturnRows(pgxmock.NewRows(providerCols))
	mock.ExpectQuery(`FROM "Model" m`).
		WillReturnRows(pgxmock.NewRows(modelCols).
			AddRow(makeModelRowWithStatus("m-off", "code-off", "p1", true, "disabled")...).
			AddRow(makeModelRowWithStatus("m-dep", "code-dep", "p1", true, "deprecated")...).
			AddRow(makeModelRowWithStatus("m-prev", "code-prev", "p1", true, "preview")...))
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows(credentialCols))
	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx := context.Background()

	out, err := l.ListEnabledModels(ctx)
	if err != nil {
		t.Fatalf("ListEnabledModels: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("catalog = %+v, want the deprecated and preview rows only", out)
	}
	for _, m := range out {
		if m.ID == "m-off" {
			t.Error("a status-disabled model must not be advertised")
		}
	}
	if _, err := l.GetModelByCode(ctx, "code-off"); !IsNotFound(err) {
		t.Errorf("status-disabled model must not resolve by code; got %v", err)
	}
	for _, code := range []string{"code-dep", "code-prev"} {
		if _, err := l.GetModelByCode(ctx, code); err != nil {
			t.Errorf("%s must stay servable — only `disabled` withdraws a model; got %v", code, err)
		}
	}
}

// TestLoadModels_OrphanProviderIsUnservable pins the COALESCE on the LEFT
// JOIN. A model row whose provider row is missing yields NULL, and the cache
// load has no WHERE clause to filter it out first: without the COALESCE the
// scan into a bool fails and the ENTIRE catalog load errors, taking every
// model down over one dangling row. Fail-safe is "this one model is not
// servable", never "the gateway has no catalog".
func TestLoadModels_OrphanProviderIsUnservable(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	orphan := makeModelRow("m-orphan", "ghost-model", "p-gone", true)
	// COALESCE(p.enabled, false) is what the driver hands us for the NULL.
	orphan[modelColIdx("p_enabled")] = false
	mock.ExpectQuery(`COALESCE\(p\.enabled, false\)`).
		WillReturnRows(pgxmock.NewRows(modelCols).
			AddRow(orphan...).
			AddRow(makeModelRow("m-ok", "live-model", "p1", true)...))
	byID, err := l.loadModels(context.Background())
	if err != nil {
		t.Fatalf("a dangling provider reference must not fail the catalog load: %v", err)
	}
	if _, ok := byID["m-orphan"]; !ok {
		t.Error("the orphan row must still load, so pricing and admin lookups can see it")
	}
	if _, err := l.GetModelByCode(context.Background(), "ghost-model"); !IsNotFound(err) {
		t.Errorf("an orphan model must not be servable; got %v", err)
	}
	if _, err := l.GetModelByCode(context.Background(), "live-model"); err != nil {
		t.Errorf("the healthy row must be unaffected; got %v", err)
	}
}

func TestListEnabledModels_SameProviderOrderedByName(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`FROM "Provider"`).
		WillReturnRows(pgxmock.NewRows(providerCols))
	mock.ExpectQuery(`FROM "Model" m`).
		WillReturnRows(pgxmock.NewRows(modelCols).
			AddRow(makeModelRow("mb", "code-b", "p1", true)...). // name = "model-mb"
			AddRow(makeModelRow("ma", "code-a", "p1", true)...)) // name = "model-ma" (sorts first)
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows(credentialCols))
	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	out, err := l.ListEnabledModels(context.Background())
	if err != nil || len(out) != 2 {
		t.Fatalf("len = %d err=%v", len(out), err)
	}
	if out[0].Name >= out[1].Name {
		t.Errorf("same-provider order broken: %s !< %s", out[0].Name, out[1].Name)
	}
}

func TestGetCredentialByID_HitAndMiss(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	primeSnapshots(t, mock, l)

	if got, err := l.GetCredentialByID(context.Background(), "c1"); err != nil || got == nil || got.ID != "c1" {
		t.Fatalf("hit: %+v err=%v", got, err)
	}
	if _, err := l.GetCredentialByID(context.Background(), "missing"); !IsNotFound(err) {
		t.Errorf("miss: %v", err)
	}
}

func TestGetCredentialForProvider_FirstEnabledActiveWins(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	primeSnapshots(t, mock, l)

	// p1 has c1 (active) and c2 (retiring). loadCredentials picks c1.
	got, err := l.GetCredentialForProvider(context.Background(), "p1")
	if err != nil || got == nil || got.ID != "c1" {
		t.Fatalf("p1 must resolve to c1 (active); got %+v err=%v", got, err)
	}
	// p2 only has c3 (disabled) → no qualifying credential.
	if _, err := l.GetCredentialForProvider(context.Background(), "p2"); !IsNotFound(err) {
		t.Errorf("p2 must miss (disabled-only); got %v", err)
	}
	// Unknown provider → not-found.
	if _, err := l.GetCredentialForProvider(context.Background(), "ghost"); !IsNotFound(err) {
		t.Errorf("ghost must miss; got %v", err)
	}
}

func TestGetCredentialForProvider_NilIndex(t *testing.T) {
	// Same distinction on the credential index, and it matters just as much:
	// this lookup failing is what the executor reports as a target it could
	// not prepare, and "provider has no credential" invites an operator to go
	// add one that is already there.
	mock, l := newMockLayer(t, Config{})
	_ = mock
	_, err := l.GetCredentialForProvider(context.Background(), "p1")
	if !IsIndexUnavailable(err) {
		t.Errorf("nil index must report index-unavailable; got %v", err)
	}
	if IsNotFound(err) {
		t.Errorf("an unloaded index must not masquerade as a missing credential; got %v", err)
	}
}

// TestGetCredentialForProvider_LoadedIndexMissIsNotFound — a provider with no
// credential row, against a loaded index, is a real absence.
func TestGetCredentialForProvider_LoadedIndexMissIsNotFound(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	primeSnapshots(t, mock, l)
	_, err := l.GetCredentialForProvider(context.Background(), "provider-with-no-credential")
	if !IsNotFound(err) {
		t.Errorf("a miss against a loaded index must be not-found; got %v", err)
	}
	if IsIndexUnavailable(err) {
		t.Errorf("a genuine miss must not be reported as an unloaded index; got %v", err)
	}
}

func TestListCredentialsForProvider_FiltersByEnabledActiveWeight(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	primeSnapshots(t, mock, l)

	// p1: c1 (active, weight>0) + c2 (retiring, excluded). Result: 1.
	out, err := l.ListCredentialsForProvider(context.Background(), "p1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 1 || out[0].ID != "c1" {
		t.Errorf("p1: want [c1]; got %+v", out)
	}
	// p2: only c3 (disabled) → empty.
	out, err = l.ListCredentialsForProvider(context.Background(), "p2")
	if err != nil || len(out) != 0 {
		t.Errorf("p2: want []; got %+v err=%v", out, err)
	}
}

func TestListCredentialsForProvider_ExcludesZeroWeight(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`FROM "Provider"`).
		WillReturnRows(pgxmock.NewRows(providerCols))
	mock.ExpectQuery(`FROM "Model" m`).
		WillReturnRows(pgxmock.NewRows(modelCols))
	zw := makeCredRow("cz", "p9", true, "active")
	zw[9] = 0 // selectionWeight = 0
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows(credentialCols).AddRow(zw...))
	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	out, err := l.ListCredentialsForProvider(context.Background(), "p9")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("zero-weight must be excluded; got %+v", out)
	}
}

func TestGetVirtualKeyByHash_LoaderError(t *testing.T) {
	mock, l := newMockLayer(t, Config{VKCapacity: 4, VKTTL: time.Minute})
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs("h-err").
		WillReturnError(errors.New("vk-boom"))
	_, err := l.GetVirtualKeyByHash(context.Background(), "h-err")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestGetProviderAndModel_BothPaths(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	primeSnapshots(t, mock, l)

	// Happy: both rows present.
	p, m, err := l.GetProviderAndModel(context.Background(), "p1", "m1")
	if err != nil || p == nil || m == nil || p.ID != "p1" || m.ID != "m1" {
		t.Fatalf("happy: p=%+v m=%+v err=%v", p, m, err)
	}
	// Missing provider short-circuits before model lookup.
	if _, _, err := l.GetProviderAndModel(context.Background(), "ghost", "m1"); !IsNotFound(err) {
		t.Errorf("ghost provider: want not-found; got %v", err)
	}
	// Provider OK, model missing.
	if _, _, err := l.GetProviderAndModel(context.Background(), "p1", "ghost"); !IsNotFound(err) {
		t.Errorf("ghost model: want not-found; got %v", err)
	}
}

func TestProvidersAll_AndCredentialsAll(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	primeSnapshots(t, mock, l)

	if got := l.ProvidersAll(); len(got) != 2 {
		t.Errorf("ProvidersAll len = %d, want 2", len(got))
	}
	if got := l.CredentialsAll(); len(got) != 3 {
		t.Errorf("CredentialsAll len = %d, want 3", len(got))
	}

	// Nil receiver / unbuilt fields → nil result, no panic.
	var nilL *Layer
	if got := nilL.ProvidersAll(); got != nil {
		t.Error("nil receiver must return nil for ProvidersAll")
	}
	if got := nilL.CredentialsAll(); got != nil {
		t.Error("nil receiver must return nil for CredentialsAll")
	}
	empty := &Layer{}
	if got := empty.ProvidersAll(); got != nil {
		t.Error("layer with nil providers must return nil")
	}
	if got := empty.CredentialsAll(); got != nil {
		t.Error("layer with nil credentials must return nil")
	}
}

func TestGetEnabledRoutingRules_AndInvalidate(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	// First call: actual query.
	mock.ExpectQuery(`FROM "RoutingRule"`).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "name", "strategyType", "config", "matchConditions",
			"priority", "pipelineStage", "fallbackChain", "retryPolicy",
			"enabled",
		}).AddRow(
			"r1", "rule-1", "weighted", []byte(`{}`), []byte(`{}`),
			100, 1, []byte(`[]`), []byte(`{}`),
			true,
		))
	rules, err := l.GetEnabledRoutingRules(context.Background())
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if len(rules) != 1 || rules[0].ID != "r1" {
		t.Fatalf("rules: %+v", rules)
	}
	// Cached: no second ExpectQuery — must hit the rulesCache.
	rules, err = l.GetEnabledRoutingRules(context.Background())
	if err != nil || len(rules) != 1 {
		t.Fatalf("cached call: %+v %v", rules, err)
	}
	// InvalidateRoutingRules forces a reload on the next call.
	l.InvalidateRoutingRules()
	mock.ExpectQuery(`FROM "RoutingRule"`).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "name", "strategyType", "config", "matchConditions",
			"priority", "pipelineStage", "fallbackChain", "retryPolicy",
			"enabled",
		}).AddRow(
			"r2", "rule-2", "weighted", []byte(`{}`), []byte(`{}`),
			50, 1, []byte(`[]`), []byte(`{}`),
			true,
		))
	rules, err = l.GetEnabledRoutingRules(context.Background())
	if err != nil || len(rules) != 1 || rules[0].ID != "r2" {
		t.Fatalf("post-invalidate: %+v %v", rules, err)
	}
}

func TestFetchModelPricing(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	primeSnapshots(t, mock, l)

	// Empty input → (nil, nil).
	got, err := l.FetchModelPricing(context.Background(), nil)
	if err != nil || got != nil {
		t.Errorf("empty input: want (nil,nil); got %v %v", got, err)
	}

	// Known model m1 has inPrice=3.0, outPrice=12.0 (per makeModelRow).
	out, err := l.FetchModelPricing(context.Background(), []string{"m1", "ghost"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[0].ModelID != "m1" || out[0].InputPricePM != 3.0 || out[0].OutputPricePM != 12.0 {
		t.Errorf("m1 pricing wrong: %+v", out[0])
	}
	// Priced carries "this model has a price row at all", which the float
	// fields cannot: they collapse an unconfigured price and a price of zero
	// onto the same 0.0. A consumer that enforces a cost cap reads this flag,
	// so a row with real prices and Priced=false is worse than useless.
	if !out[0].Priced {
		t.Errorf("m1 has prices configured but Priced=false: %+v", out[0])
	}
	// Ghost row: zero-priced sentinel, and NOT priced — it has no row at all.
	if out[1].ModelID != "ghost" || out[1].InputPricePM != 0 || out[1].OutputPricePM != 0 {
		t.Errorf("ghost must be empty-pricing row; got %+v", out[1])
	}
	if out[1].Priced {
		t.Errorf("ghost is absent from the snapshot and must not read as priced: %+v", out[1])
	}
}

// The consequence, asserted end to end rather than inferred: a row this layer
// produced has to be selectable as a quota downgrade target. SelectCheapestIndex
// skips unpriced candidates on purpose, so an unset Priced turns every candidate
// into no-affordable-model and the downgrade answers 429 with an affordable
// model sitting in the list. Production reads pricing through this layer, so
// this is the path that was dead — the store's own copy of FetchModelPricing
// set the flag, which is why every stub-backed test stayed green.
func TestFetchModelPricing_RowsAreUsableAsDowngradeCandidates(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	primeSnapshots(t, mock, l)

	out, err := l.FetchModelPricing(context.Background(), []string{"m1"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// m1 at 3.0/12.0 per million: 1000 in + 1000 out = $0.000015, far inside
	// a $1 budget. The only thing that can make this unselectable is Priced.
	estimate := quota.CostEstimate{EstimatedInputTokens: 1000, MaxOutputTokens: 1000}
	if idx := quota.SelectCheapestIndex(quota.TargetPricingFromStore(out), estimate, 1.0); idx != 0 {
		t.Errorf("SelectCheapestIndex = %d, want 0 — a priced, affordable model was rejected "+
			"as a downgrade target, which surfaces to the caller as 429 "+
			"\"no affordable model available\"; row was %+v", idx, out[0])
	}
}

func TestFetchModelPricing_NilPricePointers(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`FROM "Provider"`).
		WillReturnRows(pgxmock.NewRows(providerCols))
	// model row with nil price columns
	r := makeModelRow("mn", "no-price", "p1", true)
	r[modelColIdx("inputPricePerMillion")] = (*string)(nil)
	r[modelColIdx("outputPricePerMillion")] = (*string)(nil)
	r[modelColIdx("cachedInputReadPricePerMillion")] = (*string)(nil)
	r[modelColIdx("cachedInputWritePricePerMillion")] = (*string)(nil)
	mock.ExpectQuery(`FROM "Model" m`).
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(r...))
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows(credentialCols))
	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	out, err := l.FetchModelPricing(context.Background(), []string{"mn"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out[0].InputPricePM != 0 || out[0].OutputPricePM != 0 {
		t.Errorf("nil price pointers must zero the result; got %+v", out[0])
	}
	// And the zero must be distinguishable from a model priced at zero: this
	// one has no price configured, so it is uncountable against a cost cap.
	if out[0].Priced {
		t.Errorf("both price columns are NULL; Priced must stay false, got %+v", out[0])
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(errNotFound) {
		t.Error("errNotFound must satisfy IsNotFound")
	}
	if !IsNotFound(pgx.ErrNoRows) {
		t.Error("pgx.ErrNoRows must satisfy IsNotFound")
	}
	if IsNotFound(errors.New("other")) {
		t.Error("unrelated error must not satisfy IsNotFound")
	}
	if IsNotFound(nil) {
		t.Error("nil must not satisfy IsNotFound")
	}
}
