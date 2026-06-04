package providerstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/providers/credstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/providers/modelstore"
)

func strptr(s string) *string { return &s }
func anyArgs(n int) []any {
	a := make([]any, n)
	for i := range a {
		a[i] = pgxmock.AnyArg()
	}
	return a
}

var (
	provCols     = []string{"id", "name", "displayName", "description", "adapter_type", "baseUrl", "pathPrefix", "apiVersion", "region", "enabled", "headers", "createdAt", "updatedAt"}
	provListCols = append(append([]string{}, provCols...), "model_count")
	modelCols    = []string{
		"id", "code", "name", "description", "providerId", "providerModelId", "type", "features",
		"inputPricePerMillion", "outputPricePerMillion", "cachedInputReadPricePerMillion", "cachedInputWritePricePerMillion",
		"maxContextTokens", "maxOutputTokens", "status", "deprecationDate", "replacedBy", "aliases",
		"inputModalities", "outputModalities", "lifecycle", "capabilityJson", "enabled", "createdAt", "updatedAt",
	}
	credCols = []string{
		"id", "name", "providerId", "enabled", "rotationState", "lastRotatedAt", "lastUsedAt", "lastSuccessAt", "lastFailureAt",
		"lastFailureReason", "totalUsageCount", "expiresAt", "selectionWeight", "status", "retireAt",
		"circuitState", "circuitReason", "circuitOpenedAt", "circuitNextProbeAt", "healthStatus", "healthSuccessRate5m",
		"healthSuccessRate1h", "healthSamplesObserved", "healthDominantError", "healthTrend", "healthStatusChangedAt",
		"healthCheckedAt", "reliabilityOverrides", "createdAt", "updatedAt",
	}
)

var tNow = time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)

func provRow(id, name string) []any {
	return []any{id, name, strptr("d"), strptr("desc"), "openai", "https://api.x", "/" + name, strptr("2024"), strptr("us-east-1"), true, []byte(`{}`), tNow, tNow}
}
func provListRow(id, name string, mc int) []any { return append(provRow(id, name), mc) }
func modelRow(id, code string) []any {
	// features + aliases are NULL (nil) so the post-scan normalisation in
	// CreateProviderWithChildren (nil → empty slice) is exercised.
	return []any{id, code, "N", strptr("d"), "p1", "pm", "chat", []string(nil), (*float64)(nil), (*float64)(nil),
		(*float64)(nil), (*float64)(nil), (*int)(nil), (*int)(nil), "active", (*time.Time)(nil), (*string)(nil), []string(nil),
		[]string{"text"}, []string{"text"}, "ga", (*[]byte)(nil), true, tNow, tNow}
}

// credRow returns the 30 CredMetadataColumns values in scan order with the
// exact field types credstore.Credential expects (the column drift this test
// guards — BUGS-FOUND #5 — is precisely a count/type mismatch here).
func credRow(id string) []any {
	tp := (*time.Time)(nil)
	sp := (*string)(nil)
	fp := (*float64)(nil)
	return []any{
		id, "cred", "p1", true, sp, // id, name, providerId, enabled, rotationState
		tp, tp, tp, tp, // lastRotatedAt, lastUsedAt, lastSuccessAt, lastFailureAt
		sp, 0, tp, // lastFailureReason, totalUsageCount, expiresAt
		100, "active", tp, // selectionWeight, status, retireAt
		"closed", sp, tp, tp, // circuitState, circuitReason, circuitOpenedAt, circuitNextProbeAt
		"healthy", fp, fp, 0, // healthStatus, healthSuccessRate5m, healthSuccessRate1h, healthSamplesObserved
		sp, sp, tp, tp, // healthDominantError, healthTrend, healthStatusChangedAt, healthCheckedAt
		[]byte(`{}`), // reliabilityOverrides
		tNow, tNow,   // createdAt, updatedAt
	}
}

func newMock(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(m.Close)
	return New(m), m
}

func TestListProviders(t *testing.T) {
	s, m := newMock(t)
	enabled := true
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM "Provider"`).WithArgs("%x%", true).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m.ExpectQuery(`FROM "Provider" p`).WithArgs("%x%", true, 10, 0).
		WillReturnRows(pgxmock.NewRows(provListCols).AddRow(provListRow("p1", "openai", 3)...))
	ps, total, err := s.ListProviders(context.Background(), ListParams{Q: "x", Enabled: &enabled, Limit: 10})
	if err != nil || total != 1 || len(ps) != 1 || ps[0].ModelCount == nil || *ps[0].ModelCount != 3 {
		t.Fatalf("ListProviders: ps=%+v total=%d err=%v", ps, total, err)
	}
}

func TestListProviders_CountError(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT COUNT`).WillReturnError(errors.New("boom"))
	if _, _, err := s.ListProviders(context.Background(), ListParams{}); err == nil {
		t.Fatal("count error should surface")
	}
}

func TestListProviders_DataAndScanError(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT COUNT`).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m.ExpectQuery(`FROM "Provider" p`).WithArgs(anyArgs(2)...).WillReturnError(errors.New("q boom"))
	if _, _, err := s.ListProviders(context.Background(), ListParams{Limit: 5}); err == nil {
		t.Fatal("data query error should surface")
	}
	// scan error: row with a bad-typed enabled column
	s2, m2 := newMock(t)
	bad := provListRow("p1", "openai", 1)
	bad[9] = "not-a-bool"
	m2.ExpectQuery(`SELECT COUNT`).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m2.ExpectQuery(`FROM "Provider" p`).WithArgs(anyArgs(2)...).WillReturnRows(pgxmock.NewRows(provListCols).AddRow(bad...))
	if _, _, err := s2.ListProviders(context.Background(), ListParams{Limit: 5}); err == nil {
		t.Fatal("scan error should surface")
	}
}

func TestGetProvider(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "Provider"\s+WHERE id = \$1`).WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(provCols).AddRow(provRow("p1", "openai")...))
	p, err := s.GetProvider(context.Background(), "p1")
	if err != nil || p == nil || p.ID != "p1" {
		t.Fatalf("GetProvider: %+v %v", p, err)
	}
	m.ExpectQuery(`FROM "Provider"`).WithArgs("missing").WillReturnError(pgx.ErrNoRows)
	p, err = s.GetProvider(context.Background(), "missing")
	if err != nil || p != nil {
		t.Fatalf("missing → (nil,nil), got %+v %v", p, err)
	}
	m.ExpectQuery(`FROM "Provider"`).WithArgs("e").WillReturnError(errors.New("db"))
	if _, err := s.GetProvider(context.Background(), "e"); err == nil {
		t.Fatal("db error should surface")
	}
}

func TestCreateProvider(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`INSERT INTO "Provider"`).WithArgs(anyArgs(10)...).
		WillReturnRows(pgxmock.NewRows(provCols).AddRow(provRow("p1", "openai")...))
	p, err := s.CreateProvider(context.Background(), CreateParams{Name: "openai"})
	if err != nil || p == nil || p.Name != "openai" {
		t.Fatalf("CreateProvider: %+v %v", p, err)
	}
	m.ExpectQuery(`INSERT INTO "Provider"`).WithArgs(anyArgs(10)...).WillReturnError(errors.New("dup"))
	if _, err := s.CreateProvider(context.Background(), CreateParams{}); err == nil {
		t.Fatal("insert error should surface")
	}
}

// TestCreateProviderWithChildren_Full is the regression test for BUGS-FOUND #5:
// the credential RETURNING yields all 30 CredMetadataColumns and must bind
// cleanly (previously the inline 14-dest scan failed). Asserts the provider,
// the inserted model, and the credential all come back.
func TestCreateProviderWithChildren_Full(t *testing.T) {
	s, m := newMock(t)
	m.ExpectBegin()
	m.ExpectQuery(`INSERT INTO "Provider"`).WithArgs(anyArgs(10)...).
		WillReturnRows(pgxmock.NewRows(provCols).AddRow(provRow("p1", "openai")...))
	m.ExpectQuery(`INSERT INTO "Model"`).WithArgs(anyArgs(20)...).
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(modelRow("m1", "gpt-4o")...))
	m.ExpectQuery(`INSERT INTO "Credential"`).WithArgs(anyArgs(8)...).
		WillReturnRows(pgxmock.NewRows(credCols).AddRow(credRow("c1")...))
	m.ExpectCommit()

	p, models, cred, err := s.CreateProviderWithChildren(context.Background(),
		CreateParams{Name: "openai"},
		[]modelstore.CreateModelParams{{Code: "gpt-4o", ProviderModelID: "pm"}},
		&credstore.CreateCredentialParams{Name: "cred"})
	if err != nil {
		t.Fatalf("CreateProviderWithChildren: %v", err)
	}
	if p == nil || p.ID != "p1" || len(models) != 1 || models[0].ID != "m1" || cred == nil || cred.ID != "c1" {
		t.Fatalf("want provider+1 model+credential, got p=%+v models=%+v cred=%+v", p, models, cred)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestCreateProviderWithChildren_NoChildren(t *testing.T) {
	s, m := newMock(t)
	m.ExpectBegin()
	m.ExpectQuery(`INSERT INTO "Provider"`).WithArgs(anyArgs(10)...).
		WillReturnRows(pgxmock.NewRows(provCols).AddRow(provRow("p1", "openai")...))
	m.ExpectCommit()
	p, models, cred, err := s.CreateProviderWithChildren(context.Background(), CreateParams{Name: "openai"}, nil, nil)
	if err != nil || p == nil || len(models) != 0 || cred != nil {
		t.Fatalf("no children: p=%+v models=%v cred=%v err=%v", p, models, cred, err)
	}
}

func TestCreateProviderWithChildren_Errors(t *testing.T) {
	t.Run("begin", func(t *testing.T) {
		s, m := newMock(t)
		m.ExpectBegin().WillReturnError(errors.New("no tx"))
		if _, _, _, err := s.CreateProviderWithChildren(context.Background(), CreateParams{}, nil, nil); err == nil {
			t.Fatal("begin error should surface")
		}
	})
	t.Run("provider insert", func(t *testing.T) {
		s, m := newMock(t)
		m.ExpectBegin()
		m.ExpectQuery(`INSERT INTO "Provider"`).WithArgs(anyArgs(10)...).WillReturnError(errors.New("boom"))
		m.ExpectRollback()
		if _, _, _, err := s.CreateProviderWithChildren(context.Background(), CreateParams{}, nil, nil); err == nil {
			t.Fatal("provider insert error should surface")
		}
	})
	t.Run("model insert", func(t *testing.T) {
		s, m := newMock(t)
		m.ExpectBegin()
		m.ExpectQuery(`INSERT INTO "Provider"`).WithArgs(anyArgs(10)...).
			WillReturnRows(pgxmock.NewRows(provCols).AddRow(provRow("p1", "o")...))
		m.ExpectQuery(`INSERT INTO "Model"`).WithArgs(anyArgs(20)...).WillReturnError(errors.New("bad model"))
		m.ExpectRollback()
		if _, _, _, err := s.CreateProviderWithChildren(context.Background(), CreateParams{},
			[]modelstore.CreateModelParams{{Code: "x"}}, nil); err == nil {
			t.Fatal("model insert error should surface")
		}
	})
	t.Run("credential insert", func(t *testing.T) {
		s, m := newMock(t)
		m.ExpectBegin()
		m.ExpectQuery(`INSERT INTO "Provider"`).WithArgs(anyArgs(10)...).
			WillReturnRows(pgxmock.NewRows(provCols).AddRow(provRow("p1", "o")...))
		m.ExpectQuery(`INSERT INTO "Credential"`).WithArgs(anyArgs(8)...).WillReturnError(errors.New("bad cred"))
		m.ExpectRollback()
		if _, _, _, err := s.CreateProviderWithChildren(context.Background(), CreateParams{}, nil,
			&credstore.CreateCredentialParams{Name: "c"}); err == nil {
			t.Fatal("credential insert error should surface")
		}
	})
	t.Run("commit", func(t *testing.T) {
		s, m := newMock(t)
		m.ExpectBegin()
		m.ExpectQuery(`INSERT INTO "Provider"`).WithArgs(anyArgs(10)...).
			WillReturnRows(pgxmock.NewRows(provCols).AddRow(provRow("p1", "o")...))
		m.ExpectCommit().WillReturnError(errors.New("commit failed"))
		if _, _, _, err := s.CreateProviderWithChildren(context.Background(), CreateParams{}, nil, nil); err == nil {
			t.Fatal("commit error should surface")
		}
	})
}

func TestUpdateProvider(t *testing.T) {
	s, m := newMock(t)
	region := strptr("eu-west-1")
	apiV := strptr("2025")
	m.ExpectQuery(`UPDATE "Provider"`).WithArgs(anyArgs(13)...).
		WillReturnRows(pgxmock.NewRows(provCols).AddRow(provRow("p1", "openai")...))
	p, err := s.UpdateProvider(context.Background(), "p1", UpdateParams{Name: strptr("New"), Region: &region, APIVersion: &apiV, UpdateHeaders: true})
	if err != nil || p == nil {
		t.Fatalf("UpdateProvider: %+v %v", p, err)
	}
	m.ExpectQuery(`UPDATE "Provider"`).WithArgs(anyArgs(13)...).WillReturnError(pgx.ErrNoRows)
	if p, err := s.UpdateProvider(context.Background(), "x", UpdateParams{}); err != nil || p != nil {
		t.Fatalf("missing → (nil,nil), got %+v %v", p, err)
	}
	m.ExpectQuery(`UPDATE "Provider"`).WithArgs(anyArgs(13)...).WillReturnError(errors.New("db"))
	if _, err := s.UpdateProvider(context.Background(), "x", UpdateParams{}); err == nil {
		t.Fatal("db error should surface")
	}
}

func TestDeleteProvider(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		s, m := newMock(t)
		m.ExpectBegin()
		m.ExpectExec(`DELETE FROM "Model" WHERE "providerId"`).WithArgs("p1").WillReturnResult(pgxmock.NewResult("DELETE", 2))
		m.ExpectExec(`DELETE FROM "Provider"`).WithArgs("p1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
		m.ExpectCommit()
		if err := s.DeleteProvider(context.Background(), "p1"); err != nil {
			t.Fatalf("DeleteProvider: %v", err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		s, m := newMock(t)
		m.ExpectBegin()
		m.ExpectExec(`DELETE FROM "Model"`).WithArgs("gone").WillReturnResult(pgxmock.NewResult("DELETE", 0))
		m.ExpectExec(`DELETE FROM "Provider"`).WithArgs("gone").WillReturnResult(pgxmock.NewResult("DELETE", 0))
		m.ExpectRollback()
		if err := s.DeleteProvider(context.Background(), "gone"); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("missing → ErrNoRows, got %v", err)
		}
	})
	t.Run("begin error", func(t *testing.T) {
		s, m := newMock(t)
		m.ExpectBegin().WillReturnError(errors.New("no tx"))
		if err := s.DeleteProvider(context.Background(), "p1"); err == nil {
			t.Fatal("begin error should surface")
		}
	})
	t.Run("model delete error", func(t *testing.T) {
		s, m := newMock(t)
		m.ExpectBegin()
		m.ExpectExec(`DELETE FROM "Model"`).WithArgs("p1").WillReturnError(errors.New("fk"))
		m.ExpectRollback()
		if err := s.DeleteProvider(context.Background(), "p1"); err == nil {
			t.Fatal("model delete error should surface")
		}
	})
	t.Run("provider delete error", func(t *testing.T) {
		s, m := newMock(t)
		m.ExpectBegin()
		m.ExpectExec(`DELETE FROM "Model"`).WithArgs("p1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
		m.ExpectExec(`DELETE FROM "Provider"`).WithArgs("p1").WillReturnError(errors.New("boom"))
		m.ExpectRollback()
		if err := s.DeleteProvider(context.Background(), "p1"); err == nil {
			t.Fatal("provider delete error should surface")
		}
	})
}

func TestListProviderHealth(t *testing.T) {
	s, m := newMock(t)
	cols := []string{"providerId", "provider", "status", "rollingErrorRate", "avgLatencyMs", "sampleCount", "lastRequestAt", "lastErrorAt"}
	m.ExpectQuery(`FROM "ProviderHealth"`).WillReturnRows(pgxmock.NewRows(cols).AddRow("p1", "openai", "healthy", 0.01, 120, 50, (*time.Time)(nil), (*time.Time)(nil)))
	rows, err := s.ListProviderHealth(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Provider != "openai" {
		t.Fatalf("ListProviderHealth: %+v %v", rows, err)
	}
	m.ExpectQuery(`FROM "ProviderHealth"`).WillReturnError(errors.New("boom"))
	if _, err := s.ListProviderHealth(context.Background()); err == nil {
		t.Fatal("query error should surface")
	}
	// scan error
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM "ProviderHealth"`).WillReturnRows(pgxmock.NewRows(cols).AddRow("p1", "o", "h", "bad-float", 1, 1, nil, nil))
	if _, err := s2.ListProviderHealth(context.Background()); err == nil {
		t.Fatal("scan error should surface")
	}
}

func TestListModelPricing(t *testing.T) {
	s, m := newMock(t)
	cols := []string{"id", "modelId", "inputPricePerMillion", "outputPricePerMillion", "effectiveDate"}
	m.ExpectQuery(`FROM "ModelPricing"`).WillReturnRows(pgxmock.NewRows(cols).AddRow("pr1", "m1", 2.5, 10.0, tNow))
	rows, err := s.ListModelPricing(context.Background())
	if err != nil || len(rows) != 1 || rows[0].ModelID != "m1" {
		t.Fatalf("ListModelPricing: %+v %v", rows, err)
	}
	m.ExpectQuery(`FROM "ModelPricing"`).WillReturnError(errors.New("boom"))
	if _, err := s.ListModelPricing(context.Background()); err == nil {
		t.Fatal("query error should surface")
	}
	// scan error: bad-typed price column
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM "ModelPricing"`).WillReturnRows(pgxmock.NewRows(cols).AddRow("pr1", "m1", "not-a-float", 1.0, tNow))
	if _, err := s2.ListModelPricing(context.Background()); err == nil {
		t.Fatal("scan error should surface")
	}
}

func TestCreateModelPricing(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`INSERT INTO "ModelPricing"`).WithArgs("m1", 2.5, 10.0).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("pr1"))
	id, err := s.CreateModelPricing(context.Background(), "m1", 2.5, 10.0)
	if err != nil || id != "pr1" {
		t.Fatalf("CreateModelPricing: %q %v", id, err)
	}
	m.ExpectQuery(`INSERT INTO "ModelPricing"`).WithArgs("m1", 0.0, 0.0).WillReturnError(errors.New("boom"))
	if _, err := s.CreateModelPricing(context.Background(), "m1", 0, 0); err == nil {
		t.Fatal("insert error should surface")
	}
}

func TestDeleteModelPricing(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`DELETE FROM "ModelPricing"`).WithArgs("pr1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	n, err := s.DeleteModelPricing(context.Background(), "pr1")
	if err != nil || n != 1 {
		t.Fatalf("DeleteModelPricing: %d %v", n, err)
	}
	m.ExpectExec(`DELETE FROM "ModelPricing"`).WithArgs("x").WillReturnError(errors.New("boom"))
	if _, err := s.DeleteModelPricing(context.Background(), "x"); err == nil {
		t.Fatal("exec error should surface")
	}
}
