package cachelayer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

// primeSnapshots loads a small fixed dataset into the snapshot caches
// so the Get* lookups have something to read.
func primeSnapshots(t *testing.T, mock pgxmock.PgxPoolIface, l *Layer) {
	t.Helper()
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`FROM "Provider"`).
		WillReturnRows(pgxmock.NewRows(providerCols).
			AddRow("p1", "openai", strPtr("OpenAI"), "openai",
				"https://api.openai.com", "/v1", nil, nil, true).
			AddRow("p2", "anthropic", strPtr("Anthropic"), "anthropic",
				"https://api.anthropic.com", "/v1", nil, nil, true))
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
	// Aliases column index: 4 capability columns were appended after aliases
	// — see modelCols in helpers_test.go for the full column order.
	r[18] = aliases
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
	mock, l := newMockLayer(t, Config{})
	_ = mock
	if _, err := l.GetModelByCode(context.Background(), "gpt-4o"); !IsNotFound(err) {
		t.Errorf("nil-index must report not-found; got %v", err)
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
	mock, l := newMockLayer(t, Config{})
	_ = mock
	if _, err := l.GetCredentialForProvider(context.Background(), "p1"); !IsNotFound(err) {
		t.Errorf("nil-index must report not-found; got %v", err)
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
	// Ghost row: zero-priced sentinel.
	if out[1].ModelID != "ghost" || out[1].InputPricePM != 0 || out[1].OutputPricePM != 0 {
		t.Errorf("ghost must be empty-pricing row; got %+v", out[1])
	}
}

func TestFetchModelPricing_NilPricePointers(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`FROM "Provider"`).
		WillReturnRows(pgxmock.NewRows(providerCols))
	// model row with nil price columns
	r := makeModelRow("mn", "no-price", "p1", true)
	r[11] = (*string)(nil) // inputPricePerMillion
	r[12] = (*string)(nil) // outputPricePerMillion
	r[13] = (*string)(nil) // cachedReadPrice
	r[14] = (*string)(nil) // cachedWritePrice
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
