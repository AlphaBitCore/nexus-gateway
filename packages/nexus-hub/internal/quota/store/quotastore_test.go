package quotastore

// Unit tests for the quotastore package.
//
// These tests use pgxmock so they run with no live Postgres. The package's
// production callers pass a *pgxpool.Pool which satisfies the PgxPool
// interface; tests pass a pgxmock.PgxPoolIface through the same parameter.
//
// Coverage scope:
//   - happy paths for every helper
//   - DB-error wrap arms (pool.Exec / pool.Query failures)
//   - Scan-error arm for every iterator
//   - empty-input fast path for MarkCredentialsPendingRotation
//
// Note: the `rows.Err()` post-loop arm in each iterator is structurally
// unreachable via pgxmock — pgxmock surfaces row-iteration errors through
// the Scan call itself, not as a separate post-iteration error. Same
// observation as packages/nexus-hub/internal/traffic/chain's
// chain_pgxmock_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

// newMock returns a fresh pgxmock pool wired up for one test.
func newMock(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock new: %v", err)
	}
	t.Cleanup(mock.Close)
	return mock
}

// ptrFloat / ptrStr / ptrTime keep the literal call sites readable.
func ptrFloat(v float64) *float64 { return &v }
func ptrStr(s string) *string     { return &s }
func ptrTime(t time.Time) *time.Time {
	return &t
}

func TestExpireOverdueVirtualKeys_Happy(t *testing.T) {
	mock := newMock(t)
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 7"))

	got, err := ExpireOverdueVirtualKeys(context.Background(), mock)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 7 {
		t.Errorf("want 7 rows, got %d", got)
	}
}

func TestExpireOverdueVirtualKeys_ExecErr(t *testing.T) {
	mock := newMock(t)
	want := errors.New("boom")
	mock.ExpectExec(`UPDATE "VirtualKey"`).WillReturnError(want)

	got, err := ExpireOverdueVirtualKeys(context.Background(), mock)
	if !errors.Is(err, want) {
		t.Errorf("must wrap underlying: %v", err)
	}
	if !strings.Contains(err.Error(), "expire overdue virtual keys") {
		t.Errorf("missing prefix: %v", err)
	}
	if got != 0 {
		t.Errorf("want 0 on err, got %d", got)
	}
}

var vkExpiryCols = []string{"id", "name", "expiresAt"}

func TestListExpiringVirtualKeys_Happy(t *testing.T) {
	mock := newMock(t)
	t1 := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs(7).
		WillReturnRows(pgxmock.NewRows(vkExpiryCols).
			AddRow("vk-1", "key-one", t1).
			AddRow("vk-2", "key-two", t2))

	got, err := ListExpiringVirtualKeys(context.Background(), mock, 7)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 || got[0].ID != "vk-1" || got[1].Name != "key-two" || !got[0].ExpiresAt.Equal(t1) {
		t.Errorf("unexpected rows: %+v", got)
	}
}

func TestListExpiringVirtualKeys_EmptyResult(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs(30).
		WillReturnRows(pgxmock.NewRows(vkExpiryCols))

	got, err := ListExpiringVirtualKeys(context.Background(), mock, 30)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want empty, got %d", len(got))
	}
}

func TestListExpiringVirtualKeys_QueryErr(t *testing.T) {
	mock := newMock(t)
	want := errors.New("pool down")
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs(7).
		WillReturnError(want)

	_, err := ListExpiringVirtualKeys(context.Background(), mock, 7)
	if !errors.Is(err, want) {
		t.Errorf("must wrap: %v", err)
	}
	if !strings.Contains(err.Error(), "list expiring virtual keys") {
		t.Errorf("missing prefix: %v", err)
	}
}

func TestListExpiringVirtualKeys_ScanErr(t *testing.T) {
	mock := newMock(t)
	// Wrong column count → Scan fails on the first row.
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs(7).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).
			AddRow("vk-1", "key-one"))

	_, err := ListExpiringVirtualKeys(context.Background(), mock, 7)
	if err == nil {
		t.Fatal("want scan err")
	}
	if !strings.Contains(err.Error(), "scan virtual key expiry") {
		t.Errorf("missing scan prefix: %v", err)
	}
}

var overrideCols = []string{"id", "targetType", "targetId", "costLimitUsd"}

func TestListActiveQuotaOverrides_Happy(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`FROM "QuotaOverride"`).
		WillReturnRows(pgxmock.NewRows(overrideCols).
			AddRow("o-1", "user", "u-1", ptrFloat(50.0)).
			AddRow("o-2", "vk", "vk-9", (*float64)(nil)))

	got, err := ListActiveQuotaOverrides(context.Background(), mock)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
	if got[0].ID != "o-1" || got[0].TargetType != "user" || got[0].CostLimitUsd == nil || *got[0].CostLimitUsd != 50.0 {
		t.Errorf("row 0 unexpected: %+v", got[0])
	}
	if got[1].CostLimitUsd != nil {
		t.Errorf("row 1 cost limit should be nil, got %v", *got[1].CostLimitUsd)
	}
}

func TestListActiveQuotaOverrides_QueryErr(t *testing.T) {
	mock := newMock(t)
	want := errors.New("db gone")
	mock.ExpectQuery(`FROM "QuotaOverride"`).WillReturnError(want)

	_, err := ListActiveQuotaOverrides(context.Background(), mock)
	if !errors.Is(err, want) {
		t.Errorf("must wrap: %v", err)
	}
	if !strings.Contains(err.Error(), "list quota overrides") {
		t.Errorf("missing prefix: %v", err)
	}
}

func TestListActiveQuotaOverrides_ScanErr(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`FROM "QuotaOverride"`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("o-1"))

	_, err := ListActiveQuotaOverrides(context.Background(), mock)
	if err == nil {
		t.Fatal("want scan err")
	}
	if !strings.Contains(err.Error(), "scan quota override") {
		t.Errorf("missing prefix: %v", err)
	}
}

var policyCols = []string{"id", "scope", "organizationId", "costLimitUsd", "alertThresholds"}

func TestListEnabledQuotaPolicies_Happy(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`FROM "QuotaPolicy"`).
		WillReturnRows(pgxmock.NewRows(policyCols).
			AddRow("p-1", "user", ptrStr("org-1"), ptrFloat(100.0), json.RawMessage(`[80,95]`)).
			AddRow("p-2", "organization", (*string)(nil), (*float64)(nil), json.RawMessage(``)))

	got, err := ListEnabledQuotaPolicies(context.Background(), mock)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
	if got[0].ID != "p-1" || got[0].OrganizationID == nil || *got[0].OrganizationID != "org-1" {
		t.Errorf("row 0 unexpected: %+v", got[0])
	}
	if got[0].CostLimitUsd == nil || *got[0].CostLimitUsd != 100.0 {
		t.Errorf("row 0 cost limit unexpected: %+v", got[0].CostLimitUsd)
	}
	if string(got[0].AlertThresholds) != "[80,95]" {
		t.Errorf("alert thresholds want [80,95], got %s", string(got[0].AlertThresholds))
	}
	if got[1].OrganizationID != nil || got[1].CostLimitUsd != nil {
		t.Errorf("row 1 should have null org+cost: %+v", got[1])
	}
}

func TestListEnabledQuotaPolicies_QueryErr(t *testing.T) {
	mock := newMock(t)
	want := errors.New("nope")
	mock.ExpectQuery(`FROM "QuotaPolicy"`).WillReturnError(want)

	_, err := ListEnabledQuotaPolicies(context.Background(), mock)
	if !errors.Is(err, want) {
		t.Errorf("must wrap: %v", err)
	}
	if !strings.Contains(err.Error(), "list quota policies") {
		t.Errorf("missing prefix: %v", err)
	}
}

func TestListEnabledQuotaPolicies_ScanErr(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`FROM "QuotaPolicy"`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("p-1"))

	_, err := ListEnabledQuotaPolicies(context.Background(), mock)
	if err == nil {
		t.Fatal("want scan err")
	}
	if !strings.Contains(err.Error(), "scan quota policy") {
		t.Errorf("missing prefix: %v", err)
	}
}

func TestUpsertQuotaAlert_Happy(t *testing.T) {
	mock := newMock(t)
	expires := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	p := UpsertQuotaAlertParams{
		AlertType:       "quota.threshold",
		TargetType:      "user",
		TargetID:        "u-1",
		TargetName:      ptrStr("alice"),
		PolicyID:        ptrStr("pol-1"),
		OverrideID:      nil,
		PeriodKey:       "2026-05",
		ThresholdPct:    80,
		CurrentUsagePct: 85.5,
		CostLimitUsd:    ptrFloat(100),
		CurrentCostUsd:  ptrFloat(85.5),
		ExpiresAt:       &expires,
	}
	mock.ExpectExec(`INSERT INTO "QuotaAlert"`).
		WithArgs(
			p.AlertType, p.TargetType, p.TargetID, p.TargetName,
			p.PolicyID, p.OverrideID, p.PeriodKey, p.ThresholdPct,
			p.CurrentUsagePct, p.CostLimitUsd, p.CurrentCostUsd, p.ExpiresAt,
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	if err := UpsertQuotaAlert(context.Background(), mock, p); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func TestUpsertQuotaAlert_ExecErr(t *testing.T) {
	mock := newMock(t)
	want := errors.New("conflict")
	// 12 positional args go through the helper — match any since we're
	// asserting the error wrap, not the arg-binding behaviour.
	any12 := make([]any, 12)
	for i := range any12 {
		any12[i] = pgxmock.AnyArg()
	}
	mock.ExpectExec(`INSERT INTO "QuotaAlert"`).
		WithArgs(any12...).
		WillReturnError(want)

	err := UpsertQuotaAlert(context.Background(), mock, UpsertQuotaAlertParams{
		AlertType:    "x",
		TargetType:   "user",
		TargetID:     "u-1",
		PeriodKey:    "2026-05",
		ThresholdPct: 80,
	})
	if !errors.Is(err, want) {
		t.Errorf("must wrap: %v", err)
	}
	if !strings.Contains(err.Error(), "upsert quota alert") {
		t.Errorf("missing prefix: %v", err)
	}
}

var credCols = []string{"id", "name", "providerId", "expiresAt"}

func TestListExpiringCredentials_Happy(t *testing.T) {
	mock := newMock(t)
	exp := time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM "Credential"`).
		WithArgs(14).
		WillReturnRows(pgxmock.NewRows(credCols).
			AddRow("c-1", "openai-prod", "prov-openai", exp).
			AddRow("c-2", "anthropic-prod", "prov-anthropic", exp.Add(48*time.Hour)))

	got, err := ListExpiringCredentials(context.Background(), mock, 14)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 || got[0].ID != "c-1" || got[1].ProviderID != "prov-anthropic" {
		t.Errorf("unexpected rows: %+v", got)
	}
}

func TestListExpiringCredentials_QueryErr(t *testing.T) {
	mock := newMock(t)
	want := errors.New("conn dead")
	mock.ExpectQuery(`FROM "Credential"`).
		WithArgs(14).
		WillReturnError(want)

	_, err := ListExpiringCredentials(context.Background(), mock, 14)
	if !errors.Is(err, want) {
		t.Errorf("must wrap: %v", err)
	}
	if !strings.Contains(err.Error(), "list expiring credentials") {
		t.Errorf("missing prefix: %v", err)
	}
}

func TestListExpiringCredentials_ScanErr(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`FROM "Credential"`).
		WithArgs(14).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("c-1"))

	_, err := ListExpiringCredentials(context.Background(), mock, 14)
	if err == nil {
		t.Fatal("want scan err")
	}
	if !strings.Contains(err.Error(), "scan expiring credential") {
		t.Errorf("missing prefix: %v", err)
	}
}

func TestListOverdueCredentials_Happy(t *testing.T) {
	mock := newMock(t)
	exp := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows(credCols).
			AddRow("c-9", "stale-cred", "prov-x", exp))

	got, err := ListOverdueCredentials(context.Background(), mock)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || got[0].ID != "c-9" {
		t.Errorf("unexpected: %+v", got)
	}
}

func TestListOverdueCredentials_QueryErr(t *testing.T) {
	mock := newMock(t)
	want := errors.New("timeout")
	mock.ExpectQuery(`FROM "Credential"`).WillReturnError(want)

	_, err := ListOverdueCredentials(context.Background(), mock)
	if !errors.Is(err, want) {
		t.Errorf("must wrap: %v", err)
	}
	if !strings.Contains(err.Error(), "list overdue credentials") {
		t.Errorf("missing prefix: %v", err)
	}
}

func TestListOverdueCredentials_ScanErr(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("c-1"))

	_, err := ListOverdueCredentials(context.Background(), mock)
	if err == nil {
		t.Fatal("want scan err")
	}
	if !strings.Contains(err.Error(), "scan overdue credential") {
		t.Errorf("missing prefix: %v", err)
	}
}

func TestMarkCredentialsPendingRotation_EmptyFastPath(t *testing.T) {
	// No mock expectations — empty slice must short-circuit before any
	// pool call. Any extra Exec would fail pgxmock's expectations check.
	mock := newMock(t)
	got, err := MarkCredentialsPendingRotation(context.Background(), mock, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 0 {
		t.Errorf("want 0 rows, got %d", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected calls: %v", err)
	}
}

func TestMarkCredentialsPendingRotation_HappySingle(t *testing.T) {
	mock := newMock(t)
	mock.ExpectExec(`UPDATE "Credential"`).
		WithArgs("c-1").
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))

	got, err := MarkCredentialsPendingRotation(context.Background(), mock, []string{"c-1"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 1 {
		t.Errorf("want 1, got %d", got)
	}
}

func TestMarkCredentialsPendingRotation_HappyMulti(t *testing.T) {
	mock := newMock(t)
	mock.ExpectExec(`UPDATE "Credential"`).
		WithArgs("c-1", "c-2", "c-3").
		WillReturnResult(pgconn.NewCommandTag("UPDATE 3"))

	got, err := MarkCredentialsPendingRotation(context.Background(), mock, []string{"c-1", "c-2", "c-3"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 3 {
		t.Errorf("want 3, got %d", got)
	}
}

func TestMarkCredentialsPendingRotation_ExecErr(t *testing.T) {
	mock := newMock(t)
	want := errors.New("lock timeout")
	mock.ExpectExec(`UPDATE "Credential"`).
		WithArgs("c-1").
		WillReturnError(want)

	got, err := MarkCredentialsPendingRotation(context.Background(), mock, []string{"c-1"})
	if !errors.Is(err, want) {
		t.Errorf("must wrap: %v", err)
	}
	if !strings.Contains(err.Error(), "mark credentials pending rotation") {
		t.Errorf("missing prefix: %v", err)
	}
	if got != 0 {
		t.Errorf("want 0 on err, got %d", got)
	}
}

// ptrTime is exercised so the helper isn't dead code

func TestPtrTime(t *testing.T) {
	tm := time.Now()
	if got := ptrTime(tm); !got.Equal(tm) {
		t.Errorf("ptrTime round trip mismatch")
	}
}
