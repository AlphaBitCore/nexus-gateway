package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"
)

// modelJoinedColumns mirrors the 21-column SELECT in GetModelByCode /
// ResolveModelCandidates / ListEnabledModels. The 4 capability columns
// (inputModalities, outputModalities, lifecycle, capabilityJson) are
// appended after the base set.
var modelJoinedColumns = []string{
	"id", "code", "name", "providerId", "p_name", "adapter_type",
	"p_displayName", "p_baseUrl", "providerModelId", "type", "enabled",
	"inputPricePerMillion", "outputPricePerMillion",
	// Cache price columns surfaced through Model SELECTs so the gateway can
	// read all 4 prices from a single row (replaces the retired
	// provider_pricing regex-index seam).
	"cachedInputReadPricePerMillion", "cachedInputWritePricePerMillion",
	"features", "maxContextTokens", "maxOutputTokens", "aliases",
	"inputModalities", "outputModalities", "lifecycle", "capabilityJson",
}

func makeModelJoinedRow(id, code string) []any {
	display := "OpenAI"
	inP := "3.0"
	outP := "12.0"
	crP := "0.3"
	cwP := "3.75"
	return []any{
		id, code, "Display Name", "p1", "openai", "openai",
		&display, "https://api.openai.com", "gpt-4o", "chat", true,
		&inP, &outP,
		&crP, &cwP,
		[]string{"vision"},
		pgtype.Int4{Int32: 128000, Valid: true},
		pgtype.Int4{Int32: 16384, Valid: true},
		[]string{},
		[]string{"text"}, []string{"text"}, "ga", []byte(`{}`),
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
	})

	t.Run("missing id gets zero pricing", func(t *testing.T) {
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
