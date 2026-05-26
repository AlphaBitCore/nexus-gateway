// Pgxmock-driven Run() tests for VKExpiryJob. Covers Step 1 (expire
// overdue), Step 2 (raise expiring alerts), and Step 3 (auto-resolve
// renewed) — the full happy path plus error arms.

package expiry

import (
	"context"
	"errors"
	"testing"
	"time"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	"github.com/pashagolub/pgxmock/v4"
)

func nowPlusDays(d int) time.Time {
	return time.Now().UTC().Add(time.Duration(d) * 24 * time.Hour)
}

func TestVKExpiry_Run_Happy(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	// Step 1: ExpireOverdueVirtualKeys → UPDATE ... RETURNING (Query path).
	// ExpireOverdueVirtualKeys runs a single UPDATE returning the count.
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WillReturnResult(pgxmock.NewResult("UPDATE", 2))

	// Step 2: ListExpiringVirtualKeys SELECT.
	mock.ExpectQuery(`FROM "VirtualKey"`).WithArgs(vkExpiryAlertWindowDays).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "expiresAt"}).
			AddRow("vk-1", "key1", nowPlusDays(3)).
			AddRow("vk-2", "key2", nowPlusDays(20)))

	// Step 3: resolveRenewed SELECT — returns one firing alert NOT in
	// the expiring set so it must be resolved.
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).
		WithArgs(vkExpiringRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			AddRow("vk:vk-1").
			AddRow("vk:vk-old").
			AddRow("not-vk-format"))

	raiser := &fakeRaiser{}
	j := &VKExpiryJob{pool: mock, raiser: raiser, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}

	if len(raiser.raises) != 2 {
		t.Errorf("raises = %d, want 2", len(raiser.raises))
	}
	if len(raiser.resolves) != 1 {
		t.Errorf("resolves = %d, want 1 (vk-old)", len(raiser.resolves))
	}
	if len(raiser.resolves) == 1 && raiser.resolves[0].TargetKey != "vk:vk-old" {
		t.Errorf("resolved targetKey = %q, want vk:vk-old", raiser.resolves[0].TargetKey)
	}
}

func TestVKExpiry_Run_ExpireQueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("expire boom")
	mock.ExpectExec(`UPDATE "VirtualKey"`).WillReturnError(sentinel)
	mock.ExpectQuery(`FROM "VirtualKey"`).WithArgs(vkExpiryAlertWindowDays).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "expiresAt"}))
	// resolveRenewed still runs even if expiring list is empty.
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(vkExpiringRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	raiser := &fakeRaiser{}
	j := &VKExpiryJob{pool: mock, raiser: raiser, logger: testLogger()}
	err := j.Run(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel surface", err)
	}
}

func TestVKExpiry_Run_ListExpiringError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	sentinel := errors.New("list boom")
	mock.ExpectQuery(`FROM "VirtualKey"`).WithArgs(vkExpiryAlertWindowDays).WillReturnError(sentinel)
	// resolveRenewed should be skipped (early return on list error).

	raiser := &fakeRaiser{}
	j := &VKExpiryJob{pool: mock, raiser: raiser, logger: testLogger()}
	err := j.Run(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestVKExpiry_Run_ResolveRenewedRaiseError(t *testing.T) {
	// Cover: raiser.Raise error during Step 2 — appended to errs.
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectQuery(`FROM "VirtualKey"`).WithArgs(vkExpiryAlertWindowDays).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "expiresAt"}).
			AddRow("vk-x", "kx", nowPlusDays(5)))
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(vkExpiringRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	raiser := &errRaiser{raiseErr: errors.New("raise boom")}
	j := &VKExpiryJob{pool: mock, raiser: raiser, logger: testLogger()}
	if err := j.Run(context.Background()); err == nil {
		t.Fatalf("expected error from raise")
	}
}

func TestVKExpiry_ResolveRenewed_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("query boom")
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(vkExpiringRuleID).WillReturnError(sentinel)

	j := &VKExpiryJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	err := j.resolveRenewed(context.Background(), map[string]bool{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestVKExpiry_ResolveRenewed_ResolveError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(vkExpiringRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).AddRow("vk:vk-foo"))

	raiser := &errRaiser{resolveErr: errors.New("resolve failed")}
	j := &VKExpiryJob{pool: mock, raiser: raiser, logger: testLogger()}
	// resolveRenewed only warns on raiser failure; no error returned.
	if err := j.resolveRenewed(context.Background(), map[string]bool{}); err != nil {
		t.Errorf("unexpected err: %v", err)
	}
}

// errRaiser returns scripted errors.
type errRaiser struct {
	raiseErr   error
	resolveErr error
}

func (e *errRaiser) Raise(context.Context, alerting.RaiseInput) error {
	return e.raiseErr
}
func (e *errRaiser) Resolve(_ context.Context, _ /*ruleID*/, _ /*targetKey*/, _ /*reason*/ string) error {
	return e.resolveErr
}
