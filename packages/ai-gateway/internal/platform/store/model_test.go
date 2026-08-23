package store

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"
)

// modelJoinedColumns mirrors the provider-joined SELECT in GetModelByCode /
// ResolveModelCandidates / ListEnabledModels. The 4 capability columns
// (inputModalities, outputModalities, lifecycle, capabilityJson) are
// appended after the base set.
var modelJoinedColumns = []string{
	"id", "code", "name", "providerId", "p_name", "adapter_type",
	"p_displayName", "p_baseUrl", "providerModelId", "type", "enabled", "p_enabled", "m_status",
	"inputPricePerMillion", "outputPricePerMillion",
	// Cache price columns surfaced through Model SELECTs so the gateway can
	// read all 4 prices from a single row (replaces the retired
	// provider_pricing regex-index seam).
	"cachedInputReadPricePerMillion", "cachedInputWritePricePerMillion",
	// Audio-token rates (realtime models); NULL everywhere else.
	"audioInputPricePerMillion", "audioOutputPricePerMillion", "cachedAudioInputReadPricePerMillion",
	"features", "maxContextTokens", "maxOutputTokens", "aliases",
	"inputModalities", "outputModalities", "requiredModalities", "lifecycle", "capabilityJson",
	// AP-2: appended at the end of every model SELECT (min output tokens,
	// temperature range, family) so the row shape stays uniform across
	// GetModelByCode / ResolveModelCandidates / ListEnabledModels.
	"minOutputTokens", "temperatureMin", "temperatureMax", "family",
}

// colIdx locates a column of a fixture's SELECT list by name. Fixtures that
// patch one cell of a row address it through here: a hardcoded index silently
// lands on the neighbouring column as soon as the SELECT gains one, and the
// resulting scan error names an unrelated column.
func colIdx(cols []string, name string) int {
	for i, c := range cols {
		if c == name {
			return i
		}
	}
	panic("fixture has no column " + name)
}

func makeModelJoinedRow(id, code string) []any {
	display := "OpenAI"
	inP := "3.0"
	outP := "12.0"
	crP := "0.3"
	cwP := "3.75"
	return []any{
		id, code, "Display Name", "p1", "openai", "openai",
		&display, "https://api.openai.com", "gpt-4o", "chat", true, true, "active",
		&inP, &outP,
		&crP, &cwP,
		(*string)(nil), (*string)(nil), (*string)(nil),
		[]string{"vision"},
		pgtype.Int4{Int32: 128000, Valid: true},
		pgtype.Int4{Int32: 16384, Valid: true},
		[]string{},
		[]string{"text"}, []string{"text"}, []string{}, "ga", []byte(`{}`),
		// AP-2 trailing columns: minOutputTokens, temperatureMin, temperatureMax, family
		pgtype.Int4{Int32: 1, Valid: true},
		pgtype.Float8{Float64: 0, Valid: true},
		pgtype.Float8{Float64: 2, Valid: true},
		strPtr("gpt-4o"),
	}
}

func TestGetModelByCode(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM "Model" m\s+LEFT JOIN "Provider"`).
			WithArgs("gpt-4o").
			WillReturnRows(pgxmock.NewRows(modelJoinedColumns).
				AddRow(makeModelJoinedRow("m1", "gpt-4o")...))
		got, err := db.GetModelByCode(context.Background(), "gpt-4o")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got.Code != "gpt-4o" || got.ProviderAdapterType != "openai" {
			t.Errorf("unexpected: %+v", got)
		}
		if got.TemperatureMax == nil || *got.TemperatureMax != 2 {
			t.Errorf("temperatureMax: got %v, want 2", got.TemperatureMax)
		}
		if got.MinOutputTokens == nil || *got.MinOutputTokens != 1 {
			t.Errorf("minOutputTokens: got %v, want 1", got.MinOutputTokens)
		}
		if got.Family == nil || *got.Family != "gpt-4o" {
			t.Errorf("family: got %v, want gpt-4o", got.Family)
		}
	})

	t.Run("err wraps", func(t *testing.T) {
		mock, db := newMockDB(t)
		want := errors.New("planner err")
		mock.ExpectQuery(`FROM "Model"`).
			WithArgs("gpt-4o").
			WillReturnError(want)
		_, err := db.GetModelByCode(context.Background(), "gpt-4o")
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "get model by code") {
			t.Errorf("missing prefix: %v", err)
		}
	})
}

func TestResolveModelCandidates(t *testing.T) {
	t.Run("two candidates", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM "Model" m`).
			WithArgs("gpt-4o").
			WillReturnRows(pgxmock.NewRows(modelJoinedColumns).
				AddRow(makeModelJoinedRow("m1", "gpt-4o")...).
				AddRow(makeModelJoinedRow("m2", "gpt-4o-mini")...))
		got, err := db.ResolveModelCandidates(context.Background(), "gpt-4o")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("len = %d", len(got))
		}
	})

	t.Run("query err wraps", func(t *testing.T) {
		mock, db := newMockDB(t)
		want := errors.New("planner err")
		mock.ExpectQuery(`FROM "Model"`).
			WithArgs("x").
			WillReturnError(want)
		_, err := db.ResolveModelCandidates(context.Background(), "x")
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "resolve model candidates") {
			t.Errorf("missing prefix: %v", err)
		}
	})

	t.Run("scan err wraps", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM "Model"`).
			WithArgs("y").
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("m1"))
		_, err := db.ResolveModelCandidates(context.Background(), "y")
		if err == nil || !strings.Contains(err.Error(), "scan model candidate") {
			t.Errorf("expected scan err; got: %v", err)
		}
	})
}

// TestServableFiltering_SQLCarriesTheServablePredicate pins where the servable
// rule lives in SQL: the catalog and the strict code lookup must apply it, so
// a model withdrawn by its provider or its own status is neither offered nor
// routable. pgxmock serves a query only when the text matches the expectation,
// so dropping any conjunct makes these calls error out.
func TestServableFiltering_SQLCarriesTheServablePredicate(t *testing.T) {
	// The rule is one constant shared by both queries; asserting against it
	// rather than a re-typed literal means a rewrite of the predicate updates
	// the queries and this test together, and can never leave them disagreeing.
	want := regexp.QuoteMeta(servableModelPredicate)

	t.Run("catalog list", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`WHERE ` + want).
			WillReturnRows(pgxmock.NewRows(modelJoinedColumns).
				AddRow(makeModelJoinedRow("m1", "gpt-4o")...))
		got, err := db.ListEnabledModels(context.Background())
		if err != nil {
			t.Fatalf("ListEnabledModels must carry the servable predicate: %v", err)
		}
		if len(got) != 1 || got[0].ID != "m1" {
			t.Errorf("unexpected rows: %+v", got)
		}
	})

	t.Run("strict code lookup", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`WHERE m\.code = \$1 AND ` + want).
			WithArgs("gpt-4o").
			WillReturnRows(pgxmock.NewRows(modelJoinedColumns).
				AddRow(makeModelJoinedRow("m1", "gpt-4o")...))
		got, err := db.GetModelByCode(context.Background(), "gpt-4o")
		if err != nil {
			t.Fatalf("GetModelByCode must carry the servable predicate: %v", err)
		}
		if got.ID != "m1" {
			t.Errorf("unexpected row: %+v", got)
		}
	})
}

// TestResolveModelCandidates_KeepsUnservableModelResolvable pins the one
// deliberate exception. Candidate resolution answers "which catalog rows does
// this request string name", not "which may serve it": a rule whose whole job
// is to redirect traffic away from a disabled provider matches on that
// provider's model UUID, so filtering here would stop it ever firing.
//
// The expectation is anchored to end-of-query because pgxmock matches by
// unanchored substring — without the anchor, appending the servable predicate
// would still match and this test would pass under the very mutation it exists
// to forbid.
func TestResolveModelCandidates_KeepsUnservableModelResolvable(t *testing.T) {
	mock, db := newMockDB(t)
	dead := makeModelJoinedRow("m-dead", "embed-english-v3.0")
	dead[colIdx(modelJoinedColumns, "p_enabled")] = false
	mock.ExpectQuery(`WHERE m\.enabled = true AND \(m\.code = \$1 OR \$1 = ANY\(m\.aliases\)\)\s*$`).
		WithArgs("embed-english-v3.0").
		WillReturnRows(pgxmock.NewRows(modelJoinedColumns).AddRow(dead...))
	got, err := db.ResolveModelCandidates(context.Background(), "embed-english-v3.0")
	if err != nil {
		t.Fatalf("candidate resolution must not filter on servability: %v", err)
	}
	if len(got) != 1 || got[0].ID != "m-dead" {
		t.Fatalf("requested model must stay resolvable as a candidate; got %+v", got)
	}
	if got[0].Servable() {
		t.Error("the row must still report itself unservable so downstream refuses to dispatch it")
	}
}

func TestListEnabledModels(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM "Model" m`).
			WillReturnRows(pgxmock.NewRows(modelJoinedColumns).
				AddRow(makeModelJoinedRow("m1", "gpt-4o")...))
		got, err := db.ListEnabledModels(context.Background())
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(got) != 1 || got[0].ID != "m1" {
			t.Errorf("unexpected: %+v", got)
		}
	})

	t.Run("query err wraps", func(t *testing.T) {
		mock, db := newMockDB(t)
		want := errors.New("planner err")
		mock.ExpectQuery(`FROM "Model"`).
			WillReturnError(want)
		_, err := db.ListEnabledModels(context.Background())
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "list models") {
			t.Errorf("missing prefix: %v", err)
		}
	})

	t.Run("scan err wraps", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM "Model"`).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("m1"))
		_, err := db.ListEnabledModels(context.Background())
		if err == nil || !strings.Contains(err.Error(), "scan model") {
			t.Errorf("expected scan err; got: %v", err)
		}
	})
}

func TestFetchModelPricing(t *testing.T) {
	t.Run("empty ids fast-path", func(t *testing.T) {
		_, db := newMockDB(t)
		got, err := db.FetchModelPricing(context.Background(), nil)
		if err != nil || got != nil {
			t.Errorf("nil id list → (nil,nil); got %v %v", got, err)
		}
	})

	t.Run("happy two ids", func(t *testing.T) {
		mock, db := newMockDB(t)
		inP := "3.0"
		outP := "12.0"
		mock.ExpectQuery(`FROM "Model"\s+WHERE id = ANY\(\$1\)`).
			WithArgs([]string{"m1", "m2"}).
			WillReturnRows(pgxmock.NewRows([]string{"id", "inputPricePerMillion", "outputPricePerMillion"}).
				AddRow("m1", &inP, &outP).
				AddRow("m2", &inP, &outP))
		got, err := db.FetchModelPricing(context.Background(), []string{"m1", "m2"})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(got) != 2 || got[0].ModelID != "m1" || got[0].InputPricePM != 3.0 {
			t.Errorf("unexpected: %+v", got)
		}
		// A model with set price columns is Priced=true.
		if !got[0].Priced || !got[1].Priced {
			t.Errorf("priced rows must report Priced=true; got %+v", got)
		}
	})

	t.Run("missing id gets zero pricing and Priced=false", func(t *testing.T) {
		mock, db := newMockDB(t)
		inP := "3.0"
		outP := "12.0"
		mock.ExpectQuery(`FROM "Model"`).
			WithArgs([]string{"m1", "missing"}).
			WillReturnRows(pgxmock.NewRows([]string{"id", "inputPricePerMillion", "outputPricePerMillion"}).
				AddRow("m1", &inP, &outP))
		got, err := db.FetchModelPricing(context.Background(), []string{"m1", "missing"})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(got) != 2 || got[1].ModelID != "missing" || got[1].InputPricePM != 0 {
			t.Errorf("unexpected: %+v", got)
		}
		// A model with NO row in the result has no enforceable price —
		// it must be Priced=false so the downgrade selector skips it, while the
		// present row is Priced=true.
		if !got[0].Priced {
			t.Errorf("present row must be Priced=true; got %+v", got[0])
		}
		if got[1].Priced {
			t.Errorf("missing row must be Priced=false; got %+v", got[1])
		}
	})

	t.Run("row with NULL prices is Priced=false", func(t *testing.T) {
		// Critical case: the model HAS a row but both price columns are
		// NULL. The float fields collapse to 0, indistinguishable from a free
		// model — only Priced=false marks it unpriced so the downgrade selector
		// fails closed instead of re-pricing it to 0 and bypassing the cap.
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM "Model"`).
			WithArgs([]string{"m1"}).
			WillReturnRows(pgxmock.NewRows([]string{"id", "inputPricePerMillion", "outputPricePerMillion"}).
				AddRow("m1", nil, nil))
		got, err := db.FetchModelPricing(context.Background(), []string{"m1"})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(got) != 1 || got[0].InputPricePM != 0 || got[0].OutputPricePM != 0 {
			t.Errorf("null prices must read as 0; got %+v", got)
		}
		if got[0].Priced {
			t.Errorf("row with both prices NULL must be Priced=false; got %+v", got[0])
		}
	})

	t.Run("row with only output price set is Priced=true", func(t *testing.T) {
		// A single set price column is enough to make the model cost-enforceable.
		mock, db := newMockDB(t)
		outP := "5.0"
		mock.ExpectQuery(`FROM "Model"`).
			WithArgs([]string{"m1"}).
			WillReturnRows(pgxmock.NewRows([]string{"id", "inputPricePerMillion", "outputPricePerMillion"}).
				AddRow("m1", nil, &outP))
		got, err := db.FetchModelPricing(context.Background(), []string{"m1"})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(got) != 1 || got[0].InputPricePM != 0 || got[0].OutputPricePM != 5.0 || !got[0].Priced {
			t.Errorf("output-only price must be Priced=true; got %+v", got)
		}
	})

	t.Run("query err wraps", func(t *testing.T) {
		mock, db := newMockDB(t)
		want := errors.New("planner err")
		mock.ExpectQuery(`FROM "Model"`).
			WithArgs([]string{"m1"}).
			WillReturnError(want)
		_, err := db.FetchModelPricing(context.Background(), []string{"m1"})
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "fetch model pricing") {
			t.Errorf("missing prefix: %v", err)
		}
	})

	t.Run("scan err wraps", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM "Model"`).
			WithArgs([]string{"m1"}).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("m1"))
		_, err := db.FetchModelPricing(context.Background(), []string{"m1"})
		if err == nil || !strings.Contains(err.Error(), "scan model pricing") {
			t.Errorf("expected scan err; got: %v", err)
		}
	})
}

// TestGetModelByCode_audioRatesParse pins the audio-rate scan: a
// realtime model row with the three audio price columns set must surface
// them as parsed floats (they feed realtimeCostFormula), and a chat row
// (audio columns NULL) must leave the pointers nil.
func TestGetModelByCode_audioRatesParse(t *testing.T) {
	mock, db := newMockDB(t)
	aiP, aoP, carP := "32.0", "64.0", "0.4"
	row := makeModelJoinedRow("m-rt", "gpt-realtime-2.1")
	row[colIdx(modelJoinedColumns, "audioInputPricePerMillion")] = &aiP
	row[colIdx(modelJoinedColumns, "audioOutputPricePerMillion")] = &aoP
	row[colIdx(modelJoinedColumns, "cachedAudioInputReadPricePerMillion")] = &carP
	mock.ExpectQuery(`FROM "Model" m\s+LEFT JOIN "Provider"`).
		WithArgs("gpt-realtime-2.1").
		WillReturnRows(pgxmock.NewRows(modelJoinedColumns).AddRow(row...))
	got, err := db.GetModelByCode(context.Background(), "gpt-realtime-2.1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.AudioInputPricePM == nil || *got.AudioInputPricePM != 32.0 {
		t.Fatalf("AudioInputPricePM = %v, want 32.0", got.AudioInputPricePM)
	}
	if got.AudioOutputPricePM == nil || *got.AudioOutputPricePM != 64.0 {
		t.Fatalf("AudioOutputPricePM = %v, want 64.0", got.AudioOutputPricePM)
	}
	if got.CachedAudioInputReadPricePM == nil || *got.CachedAudioInputReadPricePM != 0.4 {
		t.Fatalf("CachedAudioInputReadPricePM = %v, want 0.4", got.CachedAudioInputReadPricePM)
	}
}
