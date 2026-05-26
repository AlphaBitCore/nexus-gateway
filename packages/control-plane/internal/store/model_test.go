package store

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// openModelTestDB returns a *DB against DATABASE_URL, skipping the test when
// it is unset. Same skip contract as interception_domain_test.go.
func openModelTestDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping model integration test")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect to DB: %v", err)
	}
	t.Cleanup(pool.Close)
	return &DB{Pool: pool, pool: pool}
}

// TestListModelsFlat_SearchByCode locks in the fix that makes the q-search
// predicate match Model.code (the customer-facing identifier such as
// "gpt-4o"). Before the fix the predicate covered name|id|description|
// providerModelId only, so the Routing preview's typeahead — which lets
// operators search by code — returned no rows. We rely on the demo seed
// having an "gpt-4o" model row.
func TestListModelsFlat_SearchByCode(t *testing.T) {
	db := openModelTestDB(t)
	ctx := context.Background()

	models, _, err := db.ListModelsFlat(ctx, ModelListParams{
		Q:      "gpt-4o",
		Limit:  10,
		Offset: 0,
	})
	if err != nil {
		t.Fatalf("ListModelsFlat by code: %v", err)
	}
	if len(models) == 0 {
		t.Fatalf("expected at least one model with code matching 'gpt-4o', got 0")
	}
	var found bool
	for _, m := range models {
		if m.Code == "gpt-4o" {
			found = true
			break
		}
	}
	if !found {
		var got []string
		for _, m := range models {
			got = append(got, m.Code)
		}
		t.Fatalf("expected exact code 'gpt-4o' in results, got codes=%v", got)
	}
}
