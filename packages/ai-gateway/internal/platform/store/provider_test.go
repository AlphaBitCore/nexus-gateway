package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"
)

var providerTestColumns = []string{
	"id", "name", "displayName", "adapter_type", "baseUrl", "pathPrefix",
	"apiVersion", "region", "enabled",
}

func makeProviderRow(id string) []any {
	display := "OpenAI"
	apiV := "2024-01"
	region := "us-east-1"
	return []any{
		id, "openai", &display, "openai", "https://api.openai.com", "/v1",
		&apiV, &region, true,
	}
}

func TestGetProvider(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM "Provider"\s+WHERE id = \$1`).
			WithArgs("p1").
			WillReturnRows(pgxmock.NewRows(providerTestColumns).AddRow(makeProviderRow("p1")...))
		got, err := db.GetProvider(context.Background(), "p1")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got == nil || got.ID != "p1" || got.AdapterType != "openai" || !got.Enabled {
			t.Errorf("unexpected: %+v", got)
		}
	})

	t.Run("err wraps", func(t *testing.T) {
		mock, db := newMockDB(t)
		want := errors.New("planner err")
		mock.ExpectQuery(`FROM "Provider"`).
			WithArgs("x").
			WillReturnError(want)
		_, err := db.GetProvider(context.Background(), "x")
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "get provider") {
			t.Errorf("missing prefix: %v", err)
		}
	})
}

var modelTestColumnsGetModel = []string{
	"id", "code", "name", "providerId", "providerModelId",
	"type", "enabled", "inputPricePerMillion", "outputPricePerMillion",
	"features", "maxContextTokens", "maxOutputTokens", "aliases",
	// Capability matrix columns appended:
	"inputModalities", "outputModalities", "lifecycle", "capabilityJson",
}

func makeModelRowGetModel(id string) []any {
	inP := "3.0"
	outP := "12.0"
	return []any{
		id, "gpt-4o", "GPT-4o", "p1", "gpt-4o",
		"chat", true, &inP, &outP,
		[]string{"vision"},
		pgtype.Int4{Int32: 128000, Valid: true},
		pgtype.Int4{Int32: 16384, Valid: true},
		[]string{"gpt-4o-2024-08-06"},
		[]string{"text"}, []string{"text"}, "ga", []byte(`{}`),
	}
}

func TestGetModel(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM "Model"\s+WHERE id = \$1`).
			WithArgs("m1").
			WillReturnRows(pgxmock.NewRows(modelTestColumnsGetModel).AddRow(makeModelRowGetModel("m1")...))
		got, err := db.GetModel(context.Background(), "m1")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got == nil || got.ID != "m1" || got.Code != "gpt-4o" || got.ProviderID != "p1" {
			t.Errorf("unexpected: %+v", got)
		}
		if got.InputPricePM == nil || *got.InputPricePM != 3.0 {
			t.Errorf("input price: %v", got.InputPricePM)
		}
		if got.MaxContextTokens == nil || *got.MaxContextTokens != 128000 {
			t.Errorf("max ctx: %v", got.MaxContextTokens)
		}
	})

	t.Run("err wraps", func(t *testing.T) {
		mock, db := newMockDB(t)
		want := errors.New("planner err")
		mock.ExpectQuery(`FROM "Model"`).
			WithArgs("x").
			WillReturnError(want)
		_, err := db.GetModel(context.Background(), "x")
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "get model") {
			t.Errorf("missing prefix: %v", err)
		}
	})

	t.Run("invalid prices stay nil", func(t *testing.T) {
		mock, db := newMockDB(t)
		row := makeModelRowGetModel("m2")
		// Replace both prices with nil string pointers
		row[7] = (*string)(nil)
		row[8] = (*string)(nil)
		row[10] = pgtype.Int4{Valid: false} // maxCtx invalid
		row[11] = pgtype.Int4{Valid: false} // maxOut invalid
		mock.ExpectQuery(`FROM "Model"`).
			WithArgs("m2").
			WillReturnRows(pgxmock.NewRows(modelTestColumnsGetModel).AddRow(row...))
		got, err := db.GetModel(context.Background(), "m2")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got.InputPricePM != nil || got.OutputPricePM != nil {
			t.Errorf("expected nil prices: %+v", got)
		}
		if got.MaxContextTokens != nil || got.MaxOutputTokens != nil {
			t.Errorf("expected nil max tokens: %+v", got)
		}
	})
}

func TestGetProviderAndModel(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM "Provider"`).
			WithArgs("p1").
			WillReturnRows(pgxmock.NewRows(providerTestColumns).AddRow(makeProviderRow("p1")...))
		mock.ExpectQuery(`FROM "Model"`).
			WithArgs("m1").
			WillReturnRows(pgxmock.NewRows(modelTestColumnsGetModel).AddRow(makeModelRowGetModel("m1")...))
		p, m, err := db.GetProviderAndModel(context.Background(), "p1", "m1")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if p.ID != "p1" || m.ID != "m1" {
			t.Errorf("mismatch p=%+v m=%+v", p, m)
		}
	})

	t.Run("provider err", func(t *testing.T) {
		mock, db := newMockDB(t)
		want := errors.New("p err")
		mock.ExpectQuery(`FROM "Provider"`).
			WithArgs("p1").
			WillReturnError(want)
		_, _, err := db.GetProviderAndModel(context.Background(), "p1", "m1")
		if !errors.Is(err, want) {
			t.Errorf("must wrap provider err; got: %v", err)
		}
	})

	t.Run("model err", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM "Provider"`).
			WithArgs("p1").
			WillReturnRows(pgxmock.NewRows(providerTestColumns).AddRow(makeProviderRow("p1")...))
		want := errors.New("m err")
		mock.ExpectQuery(`FROM "Model"`).
			WithArgs("m1").
			WillReturnError(want)
		_, _, err := db.GetProviderAndModel(context.Background(), "p1", "m1")
		if !errors.Is(err, want) {
			t.Errorf("must wrap model err; got: %v", err)
		}
	})
}

func TestIntFromPgInt4(t *testing.T) {
	if got := intFromPgInt4(pgtype.Int4{Valid: false}); got != nil {
		t.Errorf("invalid → nil; got %v", got)
	}
	got := intFromPgInt4(pgtype.Int4{Int32: 42, Valid: true})
	if got == nil || *got != 42 {
		t.Errorf("valid 42 → %v", got)
	}
}
