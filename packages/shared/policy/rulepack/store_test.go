// packages/shared/policy/rulepack/store_test.go — pgxmock-driven unit tests
// for the rule-pack persistence layer.
//
// These tests use pgxmock so they (a) never touch rows the test did
// not seed and (b) run in CI without TEST_DATABASE_URL. A destructive
// integration variant that runs broad `DELETE FROM rule_pack WHERE name
// LIKE 'test/%'` statements against the shared dev DB stays gated behind
// NEXUS_DESTRUCTIVE_TESTS=1.
//
// Every test below seeds expectations through pgxmock and asserts on
// the SQL pattern + arguments + Scan result mapping. No state escapes
// the test's own pgxmock pool.
package rulepack_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
)

func newMockStore(t *testing.T) (pgxmock.PgxPoolIface, *rulepack.Store) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return mock, rulepack.NewStoreWithPgxPool(mock)
}

func TestImportPack_HappyPath_InsertsPackAndRules(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectBeginTx(pgx.TxOptions{})
	mock.ExpectQuery(`INSERT INTO "rule_pack"`).
		WithArgs("test/pack-a", "v1.0.0", "test", "").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("pack-id-1"))
	mock.ExpectQuery(`INSERT INTO "rule"`).
		WithArgs("pack-id-1", "r1", "pi", "hard", `foo`, "", "", []string{"lbl"}).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("rule-id-1"))
	mock.ExpectQuery(`INSERT INTO "rule"`).
		WithArgs("pack-id-1", "r2", "pi", "soft", `bar`, "", "", []string(nil)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("rule-id-2"))
	mock.ExpectCommit()

	got, err := store.ImportPack(context.Background(), &rulepack.Pack{
		Name: "test/pack-a", Version: "v1.0.0", Maintainer: "test",
		Rules: []rulepack.Rule{
			{RuleID: "r1", Category: "pi", Severity: "hard", Pattern: `foo`, Labels: []string{"lbl"}},
			{RuleID: "r2", Category: "pi", Severity: "soft", Pattern: `bar`},
		},
	})
	if err != nil {
		t.Fatalf("ImportPack: %v", err)
	}
	if got.ID != "pack-id-1" {
		t.Errorf("pack ID: %q", got.ID)
	}
	if len(got.Rules) != 2 || got.Rules[0].ID != "rule-id-1" || got.Rules[1].ID != "rule-id-2" {
		t.Errorf("rule IDs not threaded: %+v", got.Rules)
	}
	if got.Rules[0].PackID != "pack-id-1" {
		t.Errorf("PackID not threaded: %+v", got.Rules[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestImportPack_DuplicateVersion_ReturnsSentinel(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectBeginTx(pgx.TxOptions{})
	mock.ExpectQuery(`INSERT INTO "rule_pack"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: "23505", Message: "duplicate"})
	mock.ExpectRollback()

	_, err := store.ImportPack(context.Background(), &rulepack.Pack{
		Name: "test/dup", Version: "v1.0.0", Maintainer: "test",
	})
	if !errors.Is(err, rulepack.ErrDuplicatePackVersion) {
		t.Errorf("expected ErrDuplicatePackVersion; got: %v", err)
	}
}

func TestImportPack_BeginTxError_Wraps(t *testing.T) {
	mock, store := newMockStore(t)
	want := errors.New("conn refused")
	mock.ExpectBeginTx(pgx.TxOptions{}).WillReturnError(want)

	_, err := store.ImportPack(context.Background(), &rulepack.Pack{})
	if !errors.Is(err, want) {
		t.Errorf("must wrap; got: %v", err)
	}
	if !strings.Contains(err.Error(), "begin tx") {
		t.Errorf("missing prefix: %v", err)
	}
}

func TestImportPack_GenericInsertError_Wraps(t *testing.T) {
	mock, store := newMockStore(t)
	want := errors.New("planner error")
	mock.ExpectBeginTx(pgx.TxOptions{})
	mock.ExpectQuery(`INSERT INTO "rule_pack"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(want)
	mock.ExpectRollback()

	_, err := store.ImportPack(context.Background(), &rulepack.Pack{})
	if !errors.Is(err, want) {
		t.Errorf("must wrap; got: %v", err)
	}
	if !strings.Contains(err.Error(), "insert pack") {
		t.Errorf("missing prefix: %v", err)
	}
}

func TestImportPack_RuleInsertError_Wraps(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectBeginTx(pgx.TxOptions{})
	mock.ExpectQuery(`INSERT INTO "rule_pack"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("pack-1"))
	mock.ExpectQuery(`INSERT INTO "rule"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("constraint"))
	mock.ExpectRollback()

	_, err := store.ImportPack(context.Background(), &rulepack.Pack{
		Rules: []rulepack.Rule{{RuleID: "r1", Severity: "hard", Pattern: "x"}},
	})
	if err == nil || !strings.Contains(err.Error(), "insert rule") {
		t.Errorf("expected insert-rule wrap; got: %v", err)
	}
}

func TestImportPack_CommitError_Wraps(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectBeginTx(pgx.TxOptions{})
	mock.ExpectQuery(`INSERT INTO "rule_pack"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("pack-1"))
	mock.ExpectCommit().WillReturnError(errors.New("commit failed"))
	mock.ExpectRollback()

	_, err := store.ImportPack(context.Background(), &rulepack.Pack{})
	if err == nil || !strings.Contains(err.Error(), "commit") {
		t.Errorf("expected commit wrap; got: %v", err)
	}
}

// ListPacks / GetPack

func TestListPacks_HappyPath(t *testing.T) {
	mock, store := newMockStore(t)
	now := time.Now()
	mock.ExpectQuery(`FROM "rule_pack" p`).
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "name", "version", "maintainer", "description", "createdAt"},
		).AddRow("p1", "test/a", "v1", "m", "desc", now).
			AddRow("p2", "test/b", "v2", "m2", "", now))

	got, err := store.ListPacks(context.Background())
	if err != nil {
		t.Fatalf("ListPacks: %v", err)
	}
	if len(got) != 2 || got[0].ID != "p1" || got[1].ID != "p2" {
		t.Errorf("unexpected list: %+v", got)
	}
}

func TestListPacks_QueryError(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`FROM "rule_pack"`).WillReturnError(errors.New("conn lost"))

	_, err := store.ListPacks(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestListPacks_ScanError(t *testing.T) {
	mock, store := newMockStore(t)
	// Wrong number of columns triggers Scan error inside the row loop.
	mock.ExpectQuery(`FROM "rule_pack"`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("p1"))

	_, err := store.ListPacks(context.Background())
	if err == nil {
		t.Error("expected scan error")
	}
}

func TestGetPack_HappyPath(t *testing.T) {
	mock, store := newMockStore(t)
	now := time.Now()
	mock.ExpectQuery(`FROM "rule_pack" WHERE id = \$1`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "name", "version", "maintainer", "description", "createdAt"},
		).AddRow("p1", "test/a", "v1", "m", "d", now))
	mock.ExpectQuery(`FROM "rule" WHERE "packId" = \$1`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "ruleId", "category", "severity", "pattern", "flags", "description", "labels"},
		).AddRow("r1", "rule-1", "pi", "hard", "foo", "", "", []string{"lbl"}))

	got, err := store.GetPack(context.Background(), "p1")
	if err != nil {
		t.Fatalf("GetPack: %v", err)
	}
	if got.ID != "p1" || len(got.Rules) != 1 || got.Rules[0].PackID != "p1" {
		t.Errorf("unexpected: %+v", got)
	}
}

func TestGetPack_PackNotFound(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`FROM "rule_pack" WHERE id = \$1`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	_, err := store.GetPack(context.Background(), "missing")
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("expected ErrNoRows; got: %v", err)
	}
}

func TestGetPack_RulesQueryError(t *testing.T) {
	mock, store := newMockStore(t)
	now := time.Now()
	mock.ExpectQuery(`FROM "rule_pack" WHERE id = \$1`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "name", "version", "maintainer", "description", "createdAt"},
		).AddRow("p1", "n", "v", "m", "", now))
	mock.ExpectQuery(`FROM "rule" WHERE "packId" = \$1`).
		WithArgs("p1").
		WillReturnError(errors.New("disconnected"))

	_, err := store.GetPack(context.Background(), "p1")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestGetPack_RuleScanError(t *testing.T) {
	mock, store := newMockStore(t)
	now := time.Now()
	mock.ExpectQuery(`FROM "rule_pack" WHERE id = \$1`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "name", "version", "maintainer", "description", "createdAt"},
		).AddRow("p1", "n", "v", "m", "", now))
	mock.ExpectQuery(`FROM "rule" WHERE "packId" = \$1`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("only-one-col"))

	_, err := store.GetPack(context.Background(), "p1")
	if err == nil {
		t.Error("expected scan error")
	}
}

// Install / UpsertOverrides

func TestInstall_HappyPath_PopulatesPackName(t *testing.T) {
	mock, store := newMockStore(t)
	now := time.Now()
	mock.ExpectQuery(`INSERT INTO "rule_pack_install"`).
		WithArgs("p1", "v1.0.0", "test-hook-1", true).
		WillReturnRows(pgxmock.NewRows([]string{"id", "installedAt"}).AddRow("inst-1", now))
	mock.ExpectQuery(`SELECT name FROM "rule_pack" WHERE id = \$1`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("test/pack-a"))

	got, err := store.Install(context.Background(), rulepack.Install{
		PackID: "p1", PinVersion: "v1.0.0", BoundHookID: "test-hook-1", Enabled: true,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got.ID != "inst-1" || got.PackName != "test/pack-a" {
		t.Errorf("unexpected: %+v", got)
	}
}

func TestInstall_PackNameLookupFails_NonFatal(t *testing.T) {
	mock, store := newMockStore(t)
	now := time.Now()
	mock.ExpectQuery(`INSERT INTO "rule_pack_install"`).
		WithArgs("p1", "v1", "h", true).
		WillReturnRows(pgxmock.NewRows([]string{"id", "installedAt"}).AddRow("inst-1", now))
	mock.ExpectQuery(`SELECT name FROM "rule_pack"`).
		WithArgs("p1").
		WillReturnError(errors.New("transient"))

	got, err := store.Install(context.Background(), rulepack.Install{
		PackID: "p1", PinVersion: "v1", BoundHookID: "h", Enabled: true,
	})
	if err != nil {
		t.Fatalf("Install should swallow PackName-lookup error: %v", err)
	}
	if got.ID != "inst-1" {
		t.Errorf("ID: %q", got.ID)
	}
	if got.PackName != "" {
		t.Errorf("PackName should be empty on lookup failure; got %q", got.PackName)
	}
}

func TestInstall_InsertError_Wraps(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`INSERT INTO "rule_pack_install"`).
		WithArgs("p1", "v1", "missing-hook", true).
		WillReturnError(errors.New("fk violation"))

	_, err := store.Install(context.Background(), rulepack.Install{
		PackID: "p1", PinVersion: "v1", BoundHookID: "missing-hook", Enabled: true,
	})
	if err == nil || !strings.Contains(err.Error(), "rulepack.Install") {
		t.Errorf("expected wrap; got: %v", err)
	}
}

func TestUpsertOverrides_HappyPath(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectExec(`INSERT INTO "rule_override"`).
		WithArgs("inst-1", "r1", true, "").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`INSERT INTO "rule_override"`).
		WithArgs("inst-1", "r2", false, "soft").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	err := store.UpsertOverrides(context.Background(), "inst-1", []rulepack.Override{
		{RuleLocalID: "r1", Disabled: true},
		{RuleLocalID: "r2", SeverityOverride: "soft"},
	})
	if err != nil {
		t.Fatalf("UpsertOverrides: %v", err)
	}
}

func TestUpsertOverrides_ExecError_Wraps(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectExec(`INSERT INTO "rule_override"`).
		WithArgs("inst-1", "r1", true, "").
		WillReturnError(errors.New("conn lost"))

	err := store.UpsertOverrides(context.Background(), "inst-1", []rulepack.Override{
		{RuleLocalID: "r1", Disabled: true},
	})
	if err == nil || !strings.Contains(err.Error(), "rulepack.UpsertOverrides") {
		t.Errorf("expected wrap; got: %v", err)
	}
}

func TestUpsertOverrides_EmptyList_NoOp(t *testing.T) {
	_, store := newMockStore(t)
	if err := store.UpsertOverrides(context.Background(), "inst-1", nil); err != nil {
		t.Errorf("empty overrides should be no-op; got: %v", err)
	}
}

func TestUpdatePack_MetadataOnly_HappyPath(t *testing.T) {
	mock, store := newMockStore(t)
	maint := "new-maintainer"
	mock.ExpectBeginTx(pgx.TxOptions{})
	mock.ExpectExec(`UPDATE "rule_pack" SET`).
		WithArgs("p1", &maint, true, "").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	err := store.UpdatePack(context.Background(), "p1", rulepack.PackUpdate{
		Maintainer:  &maint,
		Description: strPtr(""), // clearing description
	})
	if err != nil {
		t.Fatalf("UpdatePack: %v", err)
	}
}

func TestUpdatePack_MetadataUpdate_NotFound(t *testing.T) {
	mock, store := newMockStore(t)
	maint := "x"
	mock.ExpectBeginTx(pgx.TxOptions{})
	mock.ExpectExec(`UPDATE "rule_pack" SET`).
		WithArgs("missing", &maint, false, "").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectRollback()

	err := store.UpdatePack(context.Background(), "missing", rulepack.PackUpdate{Maintainer: &maint})
	if !errors.Is(err, rulepack.ErrPackNotFound) {
		t.Errorf("expected ErrPackNotFound; got: %v", err)
	}
}

func TestUpdatePack_MetadataExecError(t *testing.T) {
	mock, store := newMockStore(t)
	maint := "x"
	mock.ExpectBeginTx(pgx.TxOptions{})
	mock.ExpectExec(`UPDATE "rule_pack" SET`).
		WithArgs("p1", &maint, false, "").
		WillReturnError(errors.New("constraint"))
	mock.ExpectRollback()

	err := store.UpdatePack(context.Background(), "p1", rulepack.PackUpdate{Maintainer: &maint})
	if err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Errorf("expected metadata wrap; got: %v", err)
	}
}

func TestUpdatePack_RulesOnly_ReplacesAtomically(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectBeginTx(pgx.TxOptions{})
	// No metadata fields touched → existence-check path.
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM "rule_pack" WHERE id = \$1\)`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`DELETE FROM "rule" WHERE "packId" = \$1`).
		WithArgs("p1").
		WillReturnResult(pgxmock.NewResult("DELETE", 3))
	mock.ExpectExec(`INSERT INTO "rule"`).
		WithArgs("p1", "r1", "pi", "hard", "x", "", "", []string(nil)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	newRules := []rulepack.Rule{{RuleID: "r1", Category: "pi", Severity: "hard", Pattern: "x"}}
	err := store.UpdatePack(context.Background(), "p1", rulepack.PackUpdate{Rules: &newRules})
	if err != nil {
		t.Fatalf("UpdatePack: %v", err)
	}
}

func TestUpdatePack_RulesOnly_PackMissing(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectBeginTx(pgx.TxOptions{})
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs("missing").
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectRollback()

	newRules := []rulepack.Rule{}
	err := store.UpdatePack(context.Background(), "missing", rulepack.PackUpdate{Rules: &newRules})
	if !errors.Is(err, rulepack.ErrPackNotFound) {
		t.Errorf("expected ErrPackNotFound; got: %v", err)
	}
}

func TestUpdatePack_ExistsCheckError(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectBeginTx(pgx.TxOptions{})
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs("p1").
		WillReturnError(errors.New("conn lost"))
	mock.ExpectRollback()

	newRules := []rulepack.Rule{}
	err := store.UpdatePack(context.Background(), "p1", rulepack.PackUpdate{Rules: &newRules})
	if err == nil || !strings.Contains(err.Error(), "exists") {
		t.Errorf("expected exists wrap; got: %v", err)
	}
}

func TestUpdatePack_DeleteRulesError(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectBeginTx(pgx.TxOptions{})
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`DELETE FROM "rule"`).
		WithArgs("p1").
		WillReturnError(errors.New("locked"))
	mock.ExpectRollback()

	newRules := []rulepack.Rule{}
	err := store.UpdatePack(context.Background(), "p1", rulepack.PackUpdate{Rules: &newRules})
	if err == nil || !strings.Contains(err.Error(), "delete rules") {
		t.Errorf("expected delete-rules wrap; got: %v", err)
	}
}

func TestUpdatePack_InsertRuleError(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectBeginTx(pgx.TxOptions{})
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`DELETE FROM "rule"`).
		WithArgs("p1").
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`INSERT INTO "rule"`).
		WithArgs("p1", "r1", "", "hard", "x", "", "", []string(nil)).
		WillReturnError(errors.New("constraint"))
	mock.ExpectRollback()

	newRules := []rulepack.Rule{{RuleID: "r1", Severity: "hard", Pattern: "x"}}
	err := store.UpdatePack(context.Background(), "p1", rulepack.PackUpdate{Rules: &newRules})
	if err == nil || !strings.Contains(err.Error(), "insert rule") {
		t.Errorf("expected insert-rule wrap; got: %v", err)
	}
}

func TestUpdatePack_BeginError(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectBeginTx(pgx.TxOptions{}).WillReturnError(errors.New("conn refused"))

	err := store.UpdatePack(context.Background(), "p1", rulepack.PackUpdate{})
	if err == nil || !strings.Contains(err.Error(), "begin") {
		t.Errorf("expected begin wrap; got: %v", err)
	}
}

func TestUpdatePack_CommitError(t *testing.T) {
	mock, store := newMockStore(t)
	maint := "x"
	mock.ExpectBeginTx(pgx.TxOptions{})
	mock.ExpectExec(`UPDATE "rule_pack" SET`).
		WithArgs("p1", &maint, false, "").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit().WillReturnError(errors.New("commit failed"))
	mock.ExpectRollback()

	err := store.UpdatePack(context.Background(), "p1", rulepack.PackUpdate{Maintainer: &maint})
	if err == nil || !strings.Contains(err.Error(), "commit") {
		t.Errorf("expected commit wrap; got: %v", err)
	}
}

// DeletePack / UpdateInstall / DeleteInstall

func TestDeletePack_HappyPath(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectExec(`DELETE FROM "rule_pack" WHERE id = \$1`).
		WithArgs("p1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	if err := store.DeletePack(context.Background(), "p1"); err != nil {
		t.Fatalf("DeletePack: %v", err)
	}
}

func TestDeletePack_NotFound(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectExec(`DELETE FROM "rule_pack"`).
		WithArgs("missing").
		WillReturnResult(pgxmock.NewResult("DELETE", 0))

	if err := store.DeletePack(context.Background(), "missing"); !errors.Is(err, rulepack.ErrPackNotFound) {
		t.Errorf("expected ErrPackNotFound; got: %v", err)
	}
}

func TestDeletePack_ExecError_Wraps(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectExec(`DELETE FROM "rule_pack"`).
		WithArgs("p1").
		WillReturnError(errors.New("fk"))

	err := store.DeletePack(context.Background(), "p1")
	if err == nil || !strings.Contains(err.Error(), "rulepack.DeletePack") {
		t.Errorf("expected wrap; got: %v", err)
	}
}

func TestUpdateInstall_HappyPath(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectExec(`UPDATE "rule_pack_install" SET enabled = \$2`).
		WithArgs("inst-1", false).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if err := store.UpdateInstall(context.Background(), "inst-1", false); err != nil {
		t.Fatalf("UpdateInstall: %v", err)
	}
}

func TestUpdateInstall_NotFound(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectExec(`UPDATE "rule_pack_install"`).
		WithArgs("missing", true).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	err := store.UpdateInstall(context.Background(), "missing", true)
	if !errors.Is(err, rulepack.ErrInstallNotFound) {
		t.Errorf("expected ErrInstallNotFound; got: %v", err)
	}
}

func TestUpdateInstall_ExecError(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectExec(`UPDATE "rule_pack_install"`).
		WithArgs("inst-1", true).
		WillReturnError(errors.New("x"))

	err := store.UpdateInstall(context.Background(), "inst-1", true)
	if err == nil || !strings.Contains(err.Error(), "rulepack.UpdateInstall") {
		t.Errorf("expected wrap; got: %v", err)
	}
}

func TestDeleteInstall_HappyPath(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectExec(`DELETE FROM "rule_pack_install" WHERE id = \$1`).
		WithArgs("inst-1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	if err := store.DeleteInstall(context.Background(), "inst-1"); err != nil {
		t.Fatalf("DeleteInstall: %v", err)
	}
}

func TestDeleteInstall_NotFound(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectExec(`DELETE FROM "rule_pack_install"`).
		WithArgs("missing").
		WillReturnResult(pgxmock.NewResult("DELETE", 0))

	err := store.DeleteInstall(context.Background(), "missing")
	if !errors.Is(err, rulepack.ErrInstallNotFound) {
		t.Errorf("expected ErrInstallNotFound; got: %v", err)
	}
}

func TestDeleteInstall_ExecError(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectExec(`DELETE FROM "rule_pack_install"`).
		WithArgs("inst-1").
		WillReturnError(errors.New("x"))

	err := store.DeleteInstall(context.Background(), "inst-1")
	if err == nil || !strings.Contains(err.Error(), "rulepack.DeleteInstall") {
		t.Errorf("expected wrap; got: %v", err)
	}
}

// ListInstallsForHook / LoadEffectiveSetsForHook

func TestListInstallsForHook_HappyPath(t *testing.T) {
	mock, store := newMockStore(t)
	now := time.Now()
	mock.ExpectQuery(`FROM "rule_pack_install" i`).
		WithArgs("hook-1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "packId", "name", "pinVersion", "boundHookId", "enabled", "installedAt"},
		).AddRow("i1", "p1", "test/a", "v1", "hook-1", true, now).
			AddRow("i2", "p2", "test/b", "v1", "hook-1", false, now))

	got, err := store.ListInstallsForHook(context.Background(), "hook-1")
	if err != nil {
		t.Fatalf("ListInstallsForHook: %v", err)
	}
	if len(got) != 2 || got[0].ID != "i1" || got[0].Enabled != true || got[1].Enabled != false {
		t.Errorf("unexpected: %+v", got)
	}
}

func TestListInstallsForHook_QueryError(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`FROM "rule_pack_install"`).
		WithArgs("hook-1").
		WillReturnError(errors.New("x"))

	_, err := store.ListInstallsForHook(context.Background(), "hook-1")
	if err == nil || !strings.Contains(err.Error(), "rulepack.ListInstallsForHook") {
		t.Errorf("expected wrap; got: %v", err)
	}
}

func TestListInstallsForHook_ScanError(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`FROM "rule_pack_install"`).
		WithArgs("hook-1").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("i1"))

	_, err := store.ListInstallsForHook(context.Background(), "hook-1")
	if err == nil {
		t.Error("expected scan error")
	}
}

func TestLoadEffectiveSetsForHook_FiltersDisabled(t *testing.T) {
	mock, store := newMockStore(t)
	now := time.Now()
	// ListInstallsForHook returns two installs — only the enabled one
	// should be expanded via LoadForInstall.
	mock.ExpectQuery(`FROM "rule_pack_install" i`).
		WithArgs("hook-1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "packId", "name", "pinVersion", "boundHookId", "enabled", "installedAt"},
		).AddRow("i1", "p1", "test/a", "v1", "hook-1", true, now).
			AddRow("i2", "p2", "test/b", "v1", "hook-1", false, now))
	// LoadForInstall(i1) → install + pack + overrides
	mock.ExpectQuery(`FROM "rule_pack_install" i\s+JOIN "rule_pack" p`).
		WithArgs("i1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "packId", "name", "pinVersion", "boundHookId", "enabled", "installedAt"},
		).AddRow("i1", "p1", "test/a", "v1", "hook-1", true, now))
	mock.ExpectQuery(`FROM "rule_pack" WHERE id = \$1`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "name", "version", "maintainer", "description", "createdAt"},
		).AddRow("p1", "test/a", "v1", "m", "", now))
	mock.ExpectQuery(`FROM "rule" WHERE "packId" = \$1`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "ruleId", "category", "severity", "pattern", "flags", "description", "labels"},
		).AddRow("r1", "r1", "pi", "hard", "x", "", "", []string(nil)))
	mock.ExpectQuery(`FROM "rule_override" WHERE "installId" = \$1`).
		WithArgs("i1").
		WillReturnRows(pgxmock.NewRows([]string{"ruleLocalId", "disabled", "severityOverride"}))

	got, err := store.LoadEffectiveSetsForHook(context.Background(), "hook-1")
	if err != nil {
		t.Fatalf("LoadEffectiveSetsForHook: %v", err)
	}
	if len(got) != 1 || got[0].Install.ID != "i1" {
		t.Errorf("disabled install must be filtered; got: %+v", got)
	}
}

func TestLoadEffectiveSetsForHook_ListError(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`FROM "rule_pack_install"`).
		WithArgs("hook-1").
		WillReturnError(errors.New("conn"))

	_, err := store.LoadEffectiveSetsForHook(context.Background(), "hook-1")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadEffectiveSetsForHook_LoadForInstallError(t *testing.T) {
	mock, store := newMockStore(t)
	now := time.Now()
	mock.ExpectQuery(`FROM "rule_pack_install" i`).
		WithArgs("hook-1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "packId", "name", "pinVersion", "boundHookId", "enabled", "installedAt"},
		).AddRow("i1", "p1", "test/a", "v1", "hook-1", true, now))
	mock.ExpectQuery(`FROM "rule_pack_install" i\s+JOIN`).
		WithArgs("i1").
		WillReturnError(errors.New("install lookup failed"))

	_, err := store.LoadEffectiveSetsForHook(context.Background(), "hook-1")
	if err == nil || !strings.Contains(err.Error(), "LoadEffectiveSetsForHook") {
		t.Errorf("expected wrap; got: %v", err)
	}
}

// LoadForInstall — override-application matrix

func TestLoadForInstall_OverrideDisablesRule(t *testing.T) {
	mock, store := newMockStore(t)
	now := time.Now()
	mock.ExpectQuery(`FROM "rule_pack_install" i\s+JOIN "rule_pack" p`).
		WithArgs("i1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "packId", "name", "pinVersion", "boundHookId", "enabled", "installedAt"},
		).AddRow("i1", "p1", "test/a", "v1", "hook-1", true, now))
	mock.ExpectQuery(`FROM "rule_pack" WHERE id = \$1`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "name", "version", "maintainer", "description", "createdAt"},
		).AddRow("p1", "test/a", "v1", "m", "", now))
	mock.ExpectQuery(`FROM "rule" WHERE "packId" = \$1`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "ruleId", "category", "severity", "pattern", "flags", "description", "labels"},
		).AddRow("r1id", "r1", "pi", "hard", "x", "", "", []string(nil)).
			AddRow("r2id", "r2", "pi", "hard", "y", "", "", []string(nil)))
	mock.ExpectQuery(`FROM "rule_override" WHERE "installId" = \$1`).
		WithArgs("i1").
		WillReturnRows(pgxmock.NewRows([]string{"ruleLocalId", "disabled", "severityOverride"}).
			AddRow("r1", true, ""))

	got, err := store.LoadForInstall(context.Background(), "i1")
	if err != nil {
		t.Fatalf("LoadForInstall: %v", err)
	}
	if len(got.Pack.Rules) != 1 || got.Pack.Rules[0].RuleID != "r2" {
		t.Errorf("disabled rule must be filtered; got: %+v", got.Pack.Rules)
	}
}

func TestLoadForInstall_OverrideChangesSeverity(t *testing.T) {
	mock, store := newMockStore(t)
	now := time.Now()
	mock.ExpectQuery(`FROM "rule_pack_install" i\s+JOIN "rule_pack" p`).
		WithArgs("i2").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "packId", "name", "pinVersion", "boundHookId", "enabled", "installedAt"},
		).AddRow("i2", "p1", "test/a", "v1", "hook-1", true, now))
	mock.ExpectQuery(`FROM "rule_pack" WHERE id = \$1`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "name", "version", "maintainer", "description", "createdAt"},
		).AddRow("p1", "test/a", "v1", "m", "", now))
	mock.ExpectQuery(`FROM "rule" WHERE "packId" = \$1`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "ruleId", "category", "severity", "pattern", "flags", "description", "labels"},
		).AddRow("rid", "r1", "pi", "hard", "x", "", "", []string(nil)))
	mock.ExpectQuery(`FROM "rule_override"`).
		WithArgs("i2").
		WillReturnRows(pgxmock.NewRows([]string{"ruleLocalId", "disabled", "severityOverride"}).
			AddRow("r1", false, "soft"))

	got, err := store.LoadForInstall(context.Background(), "i2")
	if err != nil {
		t.Fatalf("LoadForInstall: %v", err)
	}
	if len(got.Pack.Rules) != 1 || got.Pack.Rules[0].Severity != "soft" {
		t.Errorf("severity override not applied: %+v", got.Pack.Rules)
	}
}

func TestLoadForInstall_NoOverrides(t *testing.T) {
	mock, store := newMockStore(t)
	now := time.Now()
	mock.ExpectQuery(`FROM "rule_pack_install" i\s+JOIN "rule_pack" p`).
		WithArgs("i3").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "packId", "name", "pinVersion", "boundHookId", "enabled", "installedAt"},
		).AddRow("i3", "p1", "test/a", "v1", "hook-1", true, now))
	mock.ExpectQuery(`FROM "rule_pack" WHERE id = \$1`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "name", "version", "maintainer", "description", "createdAt"},
		).AddRow("p1", "test/a", "v1", "m", "", now))
	mock.ExpectQuery(`FROM "rule" WHERE "packId" = \$1`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "ruleId", "category", "severity", "pattern", "flags", "description", "labels"},
		).AddRow("rid", "r1", "pi", "hard", "x", "", "", []string(nil)))
	mock.ExpectQuery(`FROM "rule_override"`).
		WithArgs("i3").
		WillReturnRows(pgxmock.NewRows([]string{"ruleLocalId", "disabled", "severityOverride"}))

	got, err := store.LoadForInstall(context.Background(), "i3")
	if err != nil {
		t.Fatalf("LoadForInstall: %v", err)
	}
	if len(got.Pack.Rules) != 1 || got.Pack.Rules[0].Severity != "hard" {
		t.Errorf("unexpected: %+v", got.Pack.Rules)
	}
}

func TestLoadForInstall_InstallNotFound(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`FROM "rule_pack_install" i\s+JOIN`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	_, err := store.LoadForInstall(context.Background(), "missing")
	if err == nil || !strings.Contains(err.Error(), "load install") {
		t.Errorf("expected install-load wrap; got: %v", err)
	}
}

func TestLoadForInstall_PackLookupError(t *testing.T) {
	mock, store := newMockStore(t)
	now := time.Now()
	mock.ExpectQuery(`FROM "rule_pack_install" i\s+JOIN`).
		WithArgs("i1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "packId", "name", "pinVersion", "boundHookId", "enabled", "installedAt"},
		).AddRow("i1", "p1", "test/a", "v1", "hook-1", true, now))
	mock.ExpectQuery(`FROM "rule_pack" WHERE id = \$1`).
		WithArgs("p1").
		WillReturnError(errors.New("conn"))

	_, err := store.LoadForInstall(context.Background(), "i1")
	if err == nil || !strings.Contains(err.Error(), "load pack") {
		t.Errorf("expected pack-load wrap; got: %v", err)
	}
}

func TestLoadForInstall_OverridesQueryError(t *testing.T) {
	mock, store := newMockStore(t)
	now := time.Now()
	mock.ExpectQuery(`FROM "rule_pack_install" i\s+JOIN`).
		WithArgs("i1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "packId", "name", "pinVersion", "boundHookId", "enabled", "installedAt"},
		).AddRow("i1", "p1", "test/a", "v1", "hook-1", true, now))
	mock.ExpectQuery(`FROM "rule_pack" WHERE id = \$1`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "name", "version", "maintainer", "description", "createdAt"},
		).AddRow("p1", "test/a", "v1", "m", "", now))
	mock.ExpectQuery(`FROM "rule" WHERE "packId" = \$1`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "ruleId", "category", "severity", "pattern", "flags", "description", "labels"}))
	mock.ExpectQuery(`FROM "rule_override"`).
		WithArgs("i1").
		WillReturnError(errors.New("disconnected"))

	_, err := store.LoadForInstall(context.Background(), "i1")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadForInstall_OverrideScanError(t *testing.T) {
	mock, store := newMockStore(t)
	now := time.Now()
	mock.ExpectQuery(`FROM "rule_pack_install" i\s+JOIN`).
		WithArgs("i1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "packId", "name", "pinVersion", "boundHookId", "enabled", "installedAt"},
		).AddRow("i1", "p1", "test/a", "v1", "hook-1", true, now))
	mock.ExpectQuery(`FROM "rule_pack" WHERE id = \$1`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "name", "version", "maintainer", "description", "createdAt"},
		).AddRow("p1", "test/a", "v1", "m", "", now))
	mock.ExpectQuery(`FROM "rule" WHERE "packId" = \$1`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "ruleId", "category", "severity", "pattern", "flags", "description", "labels"}))
	mock.ExpectQuery(`FROM "rule_override"`).
		WithArgs("i1").
		WillReturnRows(pgxmock.NewRows([]string{"ruleLocalId"}).AddRow("r1"))

	_, err := store.LoadForInstall(context.Background(), "i1")
	if err == nil {
		t.Error("expected scan error")
	}
}

// NewStore production constructor (smoke)

func TestNewStore_AcceptsNilForCompilationCoverage(t *testing.T) {
	// The production constructor takes *pgxpool.Pool. We can't open a
	// real pool in a unit test; a nil pool still produces a non-nil
	// *Store, which is enough to cover the one-statement constructor.
	store := rulepack.NewStore(nil)
	if store == nil {
		t.Fatal("NewStore returned nil")
	}
}

// strPtr is a small helper used across tests.
func strPtr(s string) *string { return &s }

// --- ImportPack / UpdatePack pattern + severity validation -----------------

// asInvalidRulesError unwraps err to *rulepack.InvalidRulesError or fails.
func asInvalidRulesError(t *testing.T, err error) *rulepack.InvalidRulesError {
	t.Helper()
	var ire *rulepack.InvalidRulesError
	if !errors.As(err, &ire) {
		t.Fatalf("expected *InvalidRulesError; got %T: %v", err, err)
	}
	return ire
}

func TestImportPack_InvalidSeverity_RejectedNoWrite(t *testing.T) {
	// A severity typo (here "blocK" — not in hard|soft|warn) must be rejected
	// before any SQL runs; otherwise it silently downgrades to non-blocking at
	// runtime. The mock asserts zero DB interaction by registering no
	// expectations: any Begin/Exec would fail ExpectationsWereMet.
	mock, store := newMockStore(t)

	_, err := store.ImportPack(context.Background(), &rulepack.Pack{
		Name: "test/p", Version: "v1.0.0", Maintainer: "t",
		Rules: []rulepack.Rule{
			{RuleID: "r1", Severity: "blocK", Pattern: `foo`},
		},
	})
	ire := asInvalidRulesError(t, err)
	if len(ire.Errors) != 1 || ire.Errors[0].RuleID != "r1" {
		t.Fatalf("expected one error for r1; got %+v", ire.Errors)
	}
	if !strings.Contains(ire.Errors[0].Reason, "invalid severity") {
		t.Errorf("reason should mention severity; got %q", ire.Errors[0].Reason)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no SQL should run on validation failure: %v", err)
	}
}

func TestImportPack_InvalidRegex_RejectedNoWrite(t *testing.T) {
	mock, store := newMockStore(t)

	_, err := store.ImportPack(context.Background(), &rulepack.Pack{
		Name: "test/p", Version: "v1.0.0", Maintainer: "t",
		Rules: []rulepack.Rule{
			{RuleID: "r1", Severity: "hard", Pattern: `(`}, // unterminated group
		},
	})
	ire := asInvalidRulesError(t, err)
	if len(ire.Errors) != 1 || !strings.Contains(ire.Errors[0].Reason, "invalid pattern") {
		t.Fatalf("expected invalid-pattern error; got %+v", ire.Errors)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no SQL should run on validation failure: %v", err)
	}
}

func TestImportPack_AllInvalidRulesListed(t *testing.T) {
	// Multiple bad rules must ALL be reported in one error so the operator
	// fixes them in a single round trip — not one-at-a-time.
	_, store := newMockStore(t)

	_, err := store.ImportPack(context.Background(), &rulepack.Pack{
		Name: "test/p", Version: "v1.0.0", Maintainer: "t",
		Rules: []rulepack.Rule{
			{RuleID: "bad-sev", Severity: "nope", Pattern: `ok`},
			{RuleID: "good", Severity: "hard", Pattern: `ok`},
			{RuleID: "bad-rx", Severity: "soft", Pattern: `[`},
			{RuleID: "empty-rx", Severity: "warn", Pattern: ``},
		},
	})
	ire := asInvalidRulesError(t, err)
	if len(ire.Errors) != 3 {
		t.Fatalf("expected 3 invalid rules reported; got %d: %+v", len(ire.Errors), ire.Errors)
	}
	gotIDs := map[string]bool{}
	for _, re := range ire.Errors {
		gotIDs[re.RuleID] = true
	}
	for _, want := range []string{"bad-sev", "bad-rx", "empty-rx"} {
		if !gotIDs[want] {
			t.Errorf("missing error for %q; got %+v", want, ire.Errors)
		}
	}
	if gotIDs["good"] {
		t.Errorf("valid rule must not be flagged; got %+v", ire.Errors)
	}
}

func TestImportPack_ErrorMessageEnumeratesRules(t *testing.T) {
	_, store := newMockStore(t)
	_, err := store.ImportPack(context.Background(), &rulepack.Pack{
		Name: "test/p", Version: "v1.0.0", Maintainer: "t",
		Rules: []rulepack.Rule{{RuleID: "r1", Severity: "x", Pattern: `(`}},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid rules") {
		t.Fatalf("Error() should enumerate invalid rules; got %v", err)
	}
}

func TestUpdatePack_InvalidRule_RejectedNoWrite(t *testing.T) {
	mock, store := newMockStore(t)

	rules := []rulepack.Rule{{RuleID: "r1", Severity: "warnn", Pattern: `ok`}}
	err := store.UpdatePack(context.Background(), "p1", rulepack.PackUpdate{Rules: &rules})
	ire := asInvalidRulesError(t, err)
	if len(ire.Errors) != 1 || ire.Errors[0].RuleID != "r1" {
		t.Fatalf("expected one error for r1; got %+v", ire.Errors)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no SQL should run on validation failure: %v", err)
	}
}

func TestUpdatePack_NilRules_SkipsValidation(t *testing.T) {
	// When Rules is nil (metadata-only update), validation is skipped and the
	// metadata path runs normally.
	mock, store := newMockStore(t)
	maint := "m"
	mock.ExpectBeginTx(pgx.TxOptions{})
	mock.ExpectExec(`UPDATE "rule_pack" SET`).
		WithArgs("p1", &maint, false, "").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	if err := store.UpdatePack(context.Background(), "p1", rulepack.PackUpdate{Maintainer: &maint}); err != nil {
		t.Fatalf("metadata-only update should not validate rules: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestImportPack_ValidWarnSeverity_Accepted(t *testing.T) {
	// "warn" is a valid authoring severity and must pass validation through to
	// the SQL path (covering the happy-path of the validation gate).
	mock, store := newMockStore(t)
	mock.ExpectBeginTx(pgx.TxOptions{})
	mock.ExpectQuery(`INSERT INTO "rule_pack"`).
		WithArgs("test/p", "v1.0.0", "t", "").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("pk"))
	mock.ExpectQuery(`INSERT INTO "rule"`).
		WithArgs("pk", "r1", "cat", "warn", `ok`, "", "", []string(nil)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("ri"))
	mock.ExpectCommit()

	_, err := store.ImportPack(context.Background(), &rulepack.Pack{
		Name: "test/p", Version: "v1.0.0", Maintainer: "t",
		Rules: []rulepack.Rule{{RuleID: "r1", Category: "cat", Severity: "warn", Pattern: `ok`}},
	})
	if err != nil {
		t.Fatalf("warn severity should be accepted: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestLatestVersionForPack_PicksHighestSemver(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`SELECT id, version FROM "rule_pack" WHERE name = \$1`).
		WithArgs("nexus/secret-leak").
		WillReturnRows(pgxmock.NewRows([]string{"id", "version"}).
			AddRow("id-100", "v1.0.0").
			AddRow("id-210", "v1.2.0").
			AddRow("id-200", "v1.10.0")) // numeric: 1.10 > 1.2 > 1.0

	id, ver, err := store.LatestVersionForPack(context.Background(), "nexus/secret-leak")
	if err != nil {
		t.Fatalf("LatestVersionForPack: %v", err)
	}
	if id != "id-200" || ver != "v1.10.0" {
		t.Errorf("got (%s,%s), want (id-200,v1.10.0)", id, ver)
	}
}

func TestLatestVersionForPack_NoVersions_NotFound(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`SELECT id, version FROM "rule_pack"`).
		WithArgs("nexus/ghost").
		WillReturnRows(pgxmock.NewRows([]string{"id", "version"}))

	_, _, err := store.LatestVersionForPack(context.Background(), "nexus/ghost")
	if !errors.Is(err, rulepack.ErrPackNotFound) {
		t.Errorf("expected ErrPackNotFound; got: %v", err)
	}
}

func TestLatestVersionForPack_QueryError(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`SELECT id, version FROM "rule_pack"`).
		WithArgs("nexus/x").
		WillReturnError(errors.New("conn lost"))

	_, _, err := store.LatestVersionForPack(context.Background(), "nexus/x")
	if err == nil || !strings.Contains(err.Error(), "rulepack.LatestVersionForPack") {
		t.Errorf("expected wrap; got: %v", err)
	}
}

func TestUpgradeInstallToLatest_AdvancesPin(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`SELECT p.name, i."pinVersion"`).
		WithArgs("inst-1").
		WillReturnRows(pgxmock.NewRows([]string{"name", "pinVersion"}).AddRow("nexus/pii", "v1.0.0"))
	mock.ExpectQuery(`SELECT id, version FROM "rule_pack" WHERE name = \$1`).
		WithArgs("nexus/pii").
		WillReturnRows(pgxmock.NewRows([]string{"id", "version"}).
			AddRow("pid-1", "v1.0.0").
			AddRow("pid-2", "v2.0.0"))
	mock.ExpectExec(`UPDATE "rule_pack_install" SET "packId" = \$2, "pinVersion" = \$3`).
		WithArgs("inst-1", "pid-2", "v2.0.0").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	res, err := store.UpgradeInstallToLatest(context.Background(), "inst-1")
	if err != nil {
		t.Fatalf("UpgradeInstallToLatest: %v", err)
	}
	if !res.Upgraded || res.FromVersion != "v1.0.0" || res.ToVersion != "v2.0.0" || res.PackName != "nexus/pii" {
		t.Errorf("unexpected result: %+v", res)
	}
}

func TestUpgradeInstallToLatest_AlreadyLatest_NoOp(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`SELECT p.name, i."pinVersion"`).
		WithArgs("inst-1").
		WillReturnRows(pgxmock.NewRows([]string{"name", "pinVersion"}).AddRow("nexus/pii", "v2.0.0"))
	mock.ExpectQuery(`SELECT id, version FROM "rule_pack"`).
		WithArgs("nexus/pii").
		WillReturnRows(pgxmock.NewRows([]string{"id", "version"}).AddRow("pid-2", "v2.0.0"))
	// No UPDATE expected — already latest.

	res, err := store.UpgradeInstallToLatest(context.Background(), "inst-1")
	if err != nil {
		t.Fatalf("UpgradeInstallToLatest: %v", err)
	}
	if res.Upgraded {
		t.Errorf("expected no-op (Upgraded=false); got %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestUpgradeInstallToLatest_InstallNotFound(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`SELECT p.name, i."pinVersion"`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	_, err := store.UpgradeInstallToLatest(context.Background(), "missing")
	if !errors.Is(err, rulepack.ErrInstallNotFound) {
		t.Errorf("expected ErrInstallNotFound; got: %v", err)
	}
}

func TestUpgradeInstallToLatest_LoadInstallError(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`SELECT p.name, i."pinVersion"`).
		WithArgs("inst-1").
		WillReturnError(errors.New("conn lost"))

	_, err := store.UpgradeInstallToLatest(context.Background(), "inst-1")
	if err == nil || !strings.Contains(err.Error(), "load install") {
		t.Errorf("expected load-install wrap; got: %v", err)
	}
}

func TestLatestVersionForPack_ScanError(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`SELECT id, version FROM "rule_pack"`).
		WithArgs("nexus/x").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("only-one-col")) // 1 col vs 2 scan dests -> scan error
	_, _, err := store.LatestVersionForPack(context.Background(), "nexus/x")
	if err == nil || !strings.Contains(err.Error(), "scan") {
		t.Errorf("expected scan-error wrap; got: %v", err)
	}
}

func TestLatestVersionForPack_RowsErr(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`SELECT id, version FROM "rule_pack"`).
		WithArgs("nexus/x").
		WillReturnRows(pgxmock.NewRows([]string{"id", "version"}).
			AddRow("id-1", "v1.0.0").RowError(0, errors.New("row stream broke")))
	_, _, err := store.LatestVersionForPack(context.Background(), "nexus/x")
	if err == nil || !strings.Contains(err.Error(), "rulepack.LatestVersionForPack") {
		t.Errorf("expected rows.Err wrap; got: %v", err)
	}
}

func TestUpgradeInstallToLatest_PackFamilyGone(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`SELECT p.name, i."pinVersion"`).
		WithArgs("inst-1").
		WillReturnRows(pgxmock.NewRows([]string{"name", "pinVersion"}).AddRow("nexus/pii", "v1.0.0"))
	mock.ExpectQuery(`SELECT id, version FROM "rule_pack"`).
		WithArgs("nexus/pii").
		WillReturnRows(pgxmock.NewRows([]string{"id", "version"})) // no versions
	_, err := store.UpgradeInstallToLatest(context.Background(), "inst-1")
	if !errors.Is(err, rulepack.ErrPackNotFound) {
		t.Errorf("expected ErrPackNotFound; got: %v", err)
	}
}

func TestUpgradeInstallToLatest_UpdateError(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`SELECT p.name, i."pinVersion"`).
		WithArgs("inst-1").
		WillReturnRows(pgxmock.NewRows([]string{"name", "pinVersion"}).AddRow("nexus/pii", "v1.0.0"))
	mock.ExpectQuery(`SELECT id, version FROM "rule_pack"`).
		WithArgs("nexus/pii").
		WillReturnRows(pgxmock.NewRows([]string{"id", "version"}).AddRow("pid-2", "v2.0.0"))
	mock.ExpectExec(`UPDATE "rule_pack_install" SET "packId"`).
		WithArgs("inst-1", "pid-2", "v2.0.0").
		WillReturnError(errors.New("conn lost"))
	_, err := store.UpgradeInstallToLatest(context.Background(), "inst-1")
	if err == nil || !strings.Contains(err.Error(), "update") {
		t.Errorf("expected update wrap; got: %v", err)
	}
}

func TestUpgradeInstallToLatest_UpdateNoRows(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`SELECT p.name, i."pinVersion"`).
		WithArgs("inst-1").
		WillReturnRows(pgxmock.NewRows([]string{"name", "pinVersion"}).AddRow("nexus/pii", "v1.0.0"))
	mock.ExpectQuery(`SELECT id, version FROM "rule_pack"`).
		WithArgs("nexus/pii").
		WillReturnRows(pgxmock.NewRows([]string{"id", "version"}).AddRow("pid-2", "v2.0.0"))
	mock.ExpectExec(`UPDATE "rule_pack_install" SET "packId"`).
		WithArgs("inst-1", "pid-2", "v2.0.0").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	_, err := store.UpgradeInstallToLatest(context.Background(), "inst-1")
	if !errors.Is(err, rulepack.ErrInstallNotFound) {
		t.Errorf("expected ErrInstallNotFound on 0 rows; got: %v", err)
	}
}
