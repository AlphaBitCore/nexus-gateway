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
func boolptr(b bool) *bool    { return &b }
func anyArgs(n int) []any {
	a := make([]any, n)
	for i := range a {
		a[i] = pgxmock.AnyArg()
	}
	return a
}

var (
	provCols     = []string{"id", "name", "displayName", "description", "adapter_type", "baseUrl", "pathPrefix", "apiVersion", "region", "enabled", "serves_responses_api", "headers", "createdAt", "updatedAt"}
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
	return provRowServes(id, name, boolptr(false))
}

// provRowServes builds a Provider row with an explicit serves_responses_api
// value so the column round-trips can be asserted (nil = adapter default).
func provRowServes(id, name string, serves *bool) []any {
	return []any{id, name, strptr("d"), strptr("desc"), "openai", "https://api.x", "/" + name, strptr("2024"), strptr("us-east-1"), true, serves, []byte(`{}`), tNow, tNow}
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
	m.ExpectQuery(`INSERT INTO "Provider"`).WithArgs(anyArgs(11)...).
		WillReturnRows(pgxmock.NewRows(provCols).AddRow(provRow("p1", "openai")...))
	p, err := s.CreateProvider(context.Background(), CreateParams{Name: "openai"})
	if err != nil || p == nil || p.Name != "openai" {
		t.Fatalf("CreateProvider: %+v %v", p, err)
	}
	m.ExpectQuery(`INSERT INTO "Provider"`).WithArgs(anyArgs(11)...).WillReturnError(errors.New("dup"))
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
	m.ExpectQuery(`INSERT INTO "Provider"`).WithArgs(anyArgs(12)...).
		WillReturnRows(pgxmock.NewRows(provCols).AddRow(provRow("p1", "openai")...))
	m.ExpectQuery(`INSERT INTO "Model"`).WithArgs(anyArgs(20)...).
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(modelRow("m1", "gpt-4o")...))
	m.ExpectQuery(`INSERT INTO "Credential"`).WithArgs(anyArgs(9)...).
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
	m.ExpectQuery(`INSERT INTO "Provider"`).WithArgs(anyArgs(12)...).
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
		m.ExpectQuery(`INSERT INTO "Provider"`).WithArgs(anyArgs(12)...).WillReturnError(errors.New("boom"))
		m.ExpectRollback()
		if _, _, _, err := s.CreateProviderWithChildren(context.Background(), CreateParams{}, nil, nil); err == nil {
			t.Fatal("provider insert error should surface")
		}
	})
	t.Run("model insert", func(t *testing.T) {
		s, m := newMock(t)
		m.ExpectBegin()
		m.ExpectQuery(`INSERT INTO "Provider"`).WithArgs(anyArgs(12)...).
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
		m.ExpectQuery(`INSERT INTO "Provider"`).WithArgs(anyArgs(12)...).
			WillReturnRows(pgxmock.NewRows(provCols).AddRow(provRow("p1", "o")...))
		m.ExpectQuery(`INSERT INTO "Credential"`).WithArgs(anyArgs(9)...).WillReturnError(errors.New("bad cred"))
		m.ExpectRollback()
		if _, _, _, err := s.CreateProviderWithChildren(context.Background(), CreateParams{}, nil,
			&credstore.CreateCredentialParams{Name: "c"}); err == nil {
			t.Fatal("credential insert error should surface")
		}
	})
	t.Run("commit", func(t *testing.T) {
		s, m := newMock(t)
		m.ExpectBegin()
		m.ExpectQuery(`INSERT INTO "Provider"`).WithArgs(anyArgs(12)...).
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
	m.ExpectQuery(`UPDATE "Provider"`).WithArgs(anyArgs(15)...).
		WillReturnRows(pgxmock.NewRows(provCols).AddRow(provRow("p1", "openai")...))
	p, err := s.UpdateProvider(context.Background(), "p1", UpdateParams{Name: strptr("New"), Region: &region, APIVersion: &apiV, UpdateHeaders: true})
	if err != nil || p == nil {
		t.Fatalf("UpdateProvider: %+v %v", p, err)
	}
	m.ExpectQuery(`UPDATE "Provider"`).WithArgs(anyArgs(15)...).WillReturnError(pgx.ErrNoRows)
	if p, err := s.UpdateProvider(context.Background(), "x", UpdateParams{}); err != nil || p != nil {
		t.Fatalf("missing → (nil,nil), got %+v %v", p, err)
	}
	m.ExpectQuery(`UPDATE "Provider"`).WithArgs(anyArgs(15)...).WillReturnError(errors.New("db"))
	if _, err := s.UpdateProvider(context.Background(), "x", UpdateParams{}); err == nil {
		t.Fatal("db error should surface")
	}
}

// TestProvider_ServesResponsesAPI_RoundTrip pins the serves_responses_api
// admin write/read path: create persists an explicit override, GET returns it,
// update sets / clears / leaves it via the three-state pointer semantic, and the
// scanned value round-trips out of RETURNING. The arg-position assertions prove
// the column value (and the apply flag) reach the SQL, not just the response.
func TestProvider_ServesResponsesAPI_RoundTrip(t *testing.T) {
	t.Run("create persists explicit false (downgrade)", func(t *testing.T) {
		s, m := newMock(t)
		// Arg $10 (0-based 9) is serves_responses_api: assert the store sends
		// the caller's pointer value, and the row scans back as false.
		args := anyArgs(11)
		args[9] = boolptr(false)
		m.ExpectQuery(`INSERT INTO "Provider"`).WithArgs(args...).
			WillReturnRows(pgxmock.NewRows(provCols).AddRow(provRowServes("p1", "openai", boolptr(false))...))
		p, err := s.CreateProvider(context.Background(), CreateParams{Name: "openai", ServesResponsesAPI: boolptr(false)})
		if err != nil || p == nil || p.ServesResponsesAPI == nil || *p.ServesResponsesAPI != false {
			t.Fatalf("create did not round-trip serves=false: p=%+v err=%v", p, err)
		}
		if err := m.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet (serves value not sent to INSERT): %v", err)
		}
	})

	t.Run("create with nil persists adapter default", func(t *testing.T) {
		s, m := newMock(t)
		args := anyArgs(11)
		args[9] = (*bool)(nil)
		m.ExpectQuery(`INSERT INTO "Provider"`).WithArgs(args...).
			WillReturnRows(pgxmock.NewRows(provCols).AddRow(provRowServes("p1", "openai", nil)...))
		p, err := s.CreateProvider(context.Background(), CreateParams{Name: "openai"})
		if err != nil || p == nil || p.ServesResponsesAPI != nil {
			t.Fatalf("create with nil should keep adapter default (nil): p=%+v err=%v", p, err)
		}
		if err := m.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet: %v", err)
		}
	})

	t.Run("get returns stored true override", func(t *testing.T) {
		s, m := newMock(t)
		m.ExpectQuery(`FROM "Provider"\s+WHERE id = \$1`).WithArgs("p1").
			WillReturnRows(pgxmock.NewRows(provCols).AddRow(provRowServes("p1", "openai", boolptr(true))...))
		p, err := s.GetProvider(context.Background(), "p1")
		if err != nil || p == nil || p.ServesResponsesAPI == nil || *p.ServesResponsesAPI != true {
			t.Fatalf("get did not return serves=true: p=%+v err=%v", p, err)
		}
	})

	t.Run("update set true sends apply=true, value=true", func(t *testing.T) {
		s, m := newMock(t)
		setTrue := boolptr(true)
		args := anyArgs(15)
		args[13] = true    // applyServesResponses
		args[14] = setTrue // servesResponsesVal
		m.ExpectQuery(`UPDATE "Provider"`).WithArgs(args...).
			WillReturnRows(pgxmock.NewRows(provCols).AddRow(provRowServes("p1", "openai", boolptr(true))...))
		p, err := s.UpdateProvider(context.Background(), "p1", UpdateParams{ServesResponsesAPI: &setTrue})
		if err != nil || p == nil || p.ServesResponsesAPI == nil || *p.ServesResponsesAPI != true {
			t.Fatalf("update set true did not round-trip: p=%+v err=%v", p, err)
		}
		if err := m.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet (set true not applied): %v", err)
		}
	})

	t.Run("update clear sends apply=true, value=nil", func(t *testing.T) {
		s, m := newMock(t)
		var clear *bool // present-but-null → clear back to adapter default
		args := anyArgs(15)
		args[13] = true
		args[14] = (*bool)(nil)
		m.ExpectQuery(`UPDATE "Provider"`).WithArgs(args...).
			WillReturnRows(pgxmock.NewRows(provCols).AddRow(provRowServes("p1", "openai", nil)...))
		p, err := s.UpdateProvider(context.Background(), "p1", UpdateParams{ServesResponsesAPI: &clear})
		if err != nil || p == nil || p.ServesResponsesAPI != nil {
			t.Fatalf("update clear should reset to adapter default (nil): p=%+v err=%v", p, err)
		}
		if err := m.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet (clear not applied): %v", err)
		}
	})

	t.Run("update without field sends apply=false (no change)", func(t *testing.T) {
		s, m := newMock(t)
		args := anyArgs(15)
		args[13] = false // applyServesResponses → CASE leaves the column untouched
		args[14] = (*bool)(nil)
		m.ExpectQuery(`UPDATE "Provider"`).WithArgs(args...).
			WillReturnRows(pgxmock.NewRows(provCols).AddRow(provRowServes("p1", "openai", boolptr(true))...))
		p, err := s.UpdateProvider(context.Background(), "p1", UpdateParams{Name: strptr("New")})
		if err != nil || p == nil {
			t.Fatalf("update no-change failed: p=%+v err=%v", p, err)
		}
		if err := m.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet (no-change apply flag wrong): %v", err)
		}
	})
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
