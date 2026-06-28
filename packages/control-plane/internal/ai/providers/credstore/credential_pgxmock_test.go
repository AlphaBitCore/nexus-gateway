package credstore

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

func anyArgs(n int) []any {
	a := make([]any, n)
	for i := range a {
		a[i] = pgxmock.AnyArg()
	}
	return a
}

var tNow = time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)

var credCols = []string{
	"id", "name", "providerId", "enabled", "rotationState",
	"lastRotatedAt", "lastUsedAt", "lastSuccessAt", "lastFailureAt",
	"lastFailureReason", "totalUsageCount", "expiresAt", "selectionWeight", "status", "retireAt",
	"circuitState", "circuitReason", "circuitOpenedAt", "circuitNextProbeAt",
	"healthStatus", "healthSuccessRate5m", "healthSuccessRate1h", "healthSamplesObserved",
	"healthDominantError", "healthTrend", "healthStatusChangedAt", "healthCheckedAt",
	"reliabilityOverrides", "createdAt", "updatedAt",
}
var credEncCols = append(append([]string{}, credCols...), "encryptedKey", "encryptionIv", "encryptionTag", "encryption_key_id")

// credVals returns the 30 metadata column values in scan order with the exact
// types Credential expects.
func credVals(id string) []any {
	tp := (*time.Time)(nil)
	sp := (*string)(nil)
	fp := (*float64)(nil)
	return []any{
		id, "cred", "p1", true, sp,
		tp, tp, tp, tp,
		sp, 7, tp, 100, "active", tp,
		"closed", sp, tp, tp,
		"healthy", fp, fp, 0,
		sp, sp, tp, tp,
		json.RawMessage(`{}`), tNow, tNow,
	}
}
func credEncVals(id string) []any {
	return append(credVals(id), "enc-key", "iv", "tag", "v1")
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

func TestClearCircuit(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`UPDATE "Credential" SET`).WithArgs("c1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	if err := s.ClearCircuit(context.Background(), "c1"); err != nil {
		t.Fatalf("ClearCircuit: %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestClearCircuit_Error(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`UPDATE "Credential" SET`).WithArgs("c1").WillReturnError(errors.New("boom"))
	if err := s.ClearCircuit(context.Background(), "c1"); err == nil {
		t.Fatal("ClearCircuit should surface the exec error")
	}
}

func TestListCredentials(t *testing.T) {
	s, m := newMock(t)
	enabled := true
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM "Credential" c`).WithArgs("p1", true, "%n%").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m.ExpectQuery(`SELECT c\..* FROM "Credential" c`).WithArgs("p1", true, "%n%", 10, 0).
		WillReturnRows(pgxmock.NewRows(credCols).AddRow(credVals("c1")...))
	creds, total, err := s.ListCredentials(context.Background(), CredentialListParams{ProviderID: "p1", Enabled: &enabled, Q: "n", Limit: 10})
	if err != nil || total != 1 || len(creds) != 1 || creds[0].ID != "c1" || creds[0].Status != "active" {
		t.Fatalf("ListCredentials: creds=%+v total=%d err=%v", creds, total, err)
	}
}

func TestListCredentials_Errors(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT COUNT`).WillReturnError(errors.New("boom"))
	if _, _, err := s.ListCredentials(context.Background(), CredentialListParams{}); err == nil {
		t.Fatal("count error should surface")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`SELECT COUNT`).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m2.ExpectQuery(`FROM "Credential" c`).WithArgs(anyArgs(2)...).WillReturnError(errors.New("q"))
	if _, _, err := s2.ListCredentials(context.Background(), CredentialListParams{Limit: 5}); err == nil {
		t.Fatal("data query error should surface")
	}
	s3, m3 := newMock(t)
	bad := credVals("c1")
	bad[3] = "not-a-bool"
	m3.ExpectQuery(`SELECT COUNT`).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m3.ExpectQuery(`FROM "Credential" c`).WithArgs(anyArgs(2)...).WillReturnRows(pgxmock.NewRows(credCols).AddRow(bad...))
	if _, _, err := s3.ListCredentials(context.Background(), CredentialListParams{Limit: 5}); err == nil {
		t.Fatal("scan error should surface")
	}
}

func TestGetCredential(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "Credential" WHERE id = \$1`).WithArgs("c1").
		WillReturnRows(pgxmock.NewRows(credCols).AddRow(credVals("c1")...))
	c, err := s.GetCredential(context.Background(), "c1")
	if err != nil || c == nil || c.ID != "c1" {
		t.Fatalf("GetCredential: %+v %v", c, err)
	}
	m.ExpectQuery(`FROM "Credential" WHERE id`).WithArgs("missing").WillReturnError(pgx.ErrNoRows)
	if c, err := s.GetCredential(context.Background(), "missing"); err != nil || c != nil {
		t.Fatalf("missing → (nil,nil), got %+v %v", c, err)
	}
	m.ExpectQuery(`FROM "Credential" WHERE id`).WithArgs("e").WillReturnError(errors.New("db"))
	if _, err := s.GetCredential(context.Background(), "e"); err == nil {
		t.Fatal("db error should surface")
	}
}

func TestGetCredentialEncrypted(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "Credential"\s+WHERE id = \$1`).WithArgs("c1").
		WillReturnRows(pgxmock.NewRows(credEncCols).AddRow(credEncVals("c1")...))
	c, err := s.GetCredentialEncrypted(context.Background(), "c1")
	if err != nil || c == nil || c.EncryptedKey != "enc-key" || c.EncryptionKeyID != "v1" {
		t.Fatalf("GetCredentialEncrypted: %+v %v", c, err)
	}
	m.ExpectQuery(`FROM "Credential"`).WithArgs("missing").WillReturnError(pgx.ErrNoRows)
	if c, err := s.GetCredentialEncrypted(context.Background(), "missing"); err != nil || c != nil {
		t.Fatalf("missing → (nil,nil), got %+v %v", c, err)
	}
	m.ExpectQuery(`FROM "Credential"`).WithArgs("e").WillReturnError(errors.New("db"))
	if _, err := s.GetCredentialEncrypted(context.Background(), "e"); err == nil {
		t.Fatal("db error should surface")
	}
}

func TestCreateCredential_Defaults(t *testing.T) {
	s, m := newMock(t)
	// keyID empty → "v1"; weight 0 → 100. The first arg is the app-side id —
	// the store generates it when CreateCredentialParams.ID is empty,
	// so AnyArg; the rest are asserted by value.
	m.ExpectQuery(`INSERT INTO "Credential"`).
		WithArgs(pgxmock.AnyArg(), "cred", "p1", "ek", "iv", "tag", "v1", true, "none", (*time.Time)(nil), 100).
		WillReturnRows(pgxmock.NewRows(credCols).AddRow(credVals("c1")...))
	c, err := s.CreateCredential(context.Background(), CreateCredentialParams{
		Name: "cred", ProviderID: "p1", EncryptedKey: "ek", EncryptionIV: "iv", EncryptionTag: "tag",
		Enabled: true, RotationState: "none",
	})
	if err != nil || c == nil || c.ID != "c1" {
		t.Fatalf("CreateCredential: %+v %v", c, err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet (defaults not applied?): %v", err)
	}
	m.ExpectQuery(`INSERT INTO "Credential"`).WithArgs(anyArgs(11)...).WillReturnError(errors.New("dup"))
	if _, err := s.CreateCredential(context.Background(), CreateCredentialParams{EncryptionKeyID: "v2", SelectionWeight: 5}); err == nil {
		t.Fatal("insert error should surface")
	}
}

func TestUpdateCredentialEncryption(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`UPDATE "Credential"`).WithArgs("c1", "ek", "iv", "tag", "v1").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	if err := s.UpdateCredentialEncryption(context.Background(), "c1", "ek", "iv", "tag", ""); err != nil {
		t.Fatalf("UpdateCredentialEncryption (keyID default v1): %v", err)
	}
	m.ExpectExec(`UPDATE "Credential"`).WithArgs("c1", "ek", "iv", "tag", "v2").WillReturnError(errors.New("boom"))
	if err := s.UpdateCredentialEncryption(context.Background(), "c1", "ek", "iv", "tag", "v2"); err == nil {
		t.Fatal("exec error should surface")
	}
}

func TestUpdateCredentialMetadata(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`UPDATE "Credential" SET`).WithArgs(anyArgs(11)...).
		WillReturnRows(pgxmock.NewRows(credCols).AddRow(credVals("c1")...))
	c, err := s.UpdateCredentialMetadata(context.Background(), "c1", UpdateCredentialParams{Name: sp("New"), UpdateExpiresAt: true})
	if err != nil || c == nil {
		t.Fatalf("UpdateCredentialMetadata: %+v %v", c, err)
	}
	m.ExpectQuery(`UPDATE "Credential"`).WithArgs(anyArgs(11)...).WillReturnError(errors.New("boom"))
	if _, err := s.UpdateCredentialMetadata(context.Background(), "c1", UpdateCredentialParams{}); err == nil {
		t.Fatal("update error should surface")
	}
}

func TestCountCredentialsNotOnKey(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM "Credential" WHERE encryption_key_id != \$1`).WithArgs("v2").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(4))
	n, err := s.CountCredentialsNotOnKey(context.Background(), "v2")
	if err != nil || n != 4 {
		t.Fatalf("CountCredentialsNotOnKey: %d %v", n, err)
	}
	m.ExpectQuery(`FROM "Credential" WHERE encryption_key_id`).WithArgs("v2").WillReturnError(errors.New("boom"))
	if _, err := s.CountCredentialsNotOnKey(context.Background(), "v2"); err == nil {
		t.Fatal("count error should surface")
	}
}

func TestListCredentialsForRotation(t *testing.T) {
	s, m := newMock(t)
	// First batch: no exclusions yet — a nil excludeIDs is normalized to an
	// empty (non-nil) slice so `id <> ALL('{}')` matches every row.
	m.ExpectQuery(`FROM "Credential"\s+WHERE encryption_key_id != \$1\s+AND id <> ALL\(\$2\)`).
		WithArgs("v2", []string{}, 50).
		WillReturnRows(pgxmock.NewRows(credEncCols).AddRow(credEncVals("c1")...).AddRow(credEncVals("c2")...))
	creds, err := s.ListCredentialsForRotation(context.Background(), "v2", nil, 50)
	if err != nil || len(creds) != 2 || creds[0].EncryptedKey != "enc-key" {
		t.Fatalf("ListCredentialsForRotation: %+v %v", creds, err)
	}
	// Subsequent batch with a non-empty exclusion set: the stuck IDs are bound
	// to $2 so those rows are never re-selected.
	m.ExpectQuery(`WHERE encryption_key_id !=`).WithArgs("v2", []string{"stuck-1"}, 50).
		WillReturnRows(pgxmock.NewRows(credEncCols).AddRow(credEncVals("c3")...))
	creds, err = s.ListCredentialsForRotation(context.Background(), "v2", []string{"stuck-1"}, 50)
	if err != nil || len(creds) != 1 || creds[0].ID != "c3" {
		t.Fatalf("ListCredentialsForRotation excluded: %+v %v", creds, err)
	}
	m.ExpectQuery(`WHERE encryption_key_id !=`).WithArgs("v2", []string{}, 50).WillReturnError(errors.New("boom"))
	if _, err := s.ListCredentialsForRotation(context.Background(), "v2", nil, 50); err == nil {
		t.Fatal("query error should surface")
	}
	s2, m2 := newMock(t)
	bad := credEncVals("c1")
	bad[3] = "not-a-bool"
	m2.ExpectQuery(`WHERE encryption_key_id !=`).WithArgs("v2", []string{}, 50).WillReturnRows(pgxmock.NewRows(credEncCols).AddRow(bad...))
	if _, err := s2.ListCredentialsForRotation(context.Background(), "v2", nil, 50); err == nil {
		t.Fatal("scan error should surface")
	}
}

func TestSetCredentialReliabilityOverrides(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`UPDATE "Credential"`).WithArgs("c1", []byte(`{"x":1}`)).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	if err := s.SetCredentialReliabilityOverrides(context.Background(), "c1", []byte(`{"x":1}`)); err != nil {
		t.Fatalf("SetCredentialReliabilityOverrides: %v", err)
	}
	m.ExpectExec(`UPDATE "Credential"`).WithArgs("gone", pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	if err := s.SetCredentialReliabilityOverrides(context.Background(), "gone", nil); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing → ErrNoRows, got %v", err)
	}
	m.ExpectExec(`UPDATE "Credential"`).WithArgs("c1", pgxmock.AnyArg()).WillReturnError(errors.New("boom"))
	if err := s.SetCredentialReliabilityOverrides(context.Background(), "c1", nil); err == nil {
		t.Fatal("exec error should surface")
	}
}

func TestDeleteCredential(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`DELETE FROM "Credential" WHERE id = \$1`).WithArgs("c1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := s.DeleteCredential(context.Background(), "c1"); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	m.ExpectExec(`DELETE FROM "Credential"`).WithArgs("gone").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	if err := s.DeleteCredential(context.Background(), "gone"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing → ErrNoRows, got %v", err)
	}
	m.ExpectExec(`DELETE FROM "Credential"`).WithArgs("c1").WillReturnError(errors.New("fk"))
	if err := s.DeleteCredential(context.Background(), "c1"); err == nil {
		t.Fatal("exec error should surface")
	}
}

func TestScanCredentialRow_Exported(t *testing.T) {
	s, m := newMock(t)
	_ = s
	m.ExpectQuery(`x`).WillReturnError(pgx.ErrNoRows)
	// ScanCredentialRow maps ErrNoRows → (nil,nil); exercise via a QueryRow.
	c, err := ScanCredentialRow(m.QueryRow(context.Background(), "x"))
	if err != nil || c != nil {
		t.Fatalf("ScanCredentialRow ErrNoRows → (nil,nil), got %+v %v", c, err)
	}
}

func sp(s string) *string { return &s }
