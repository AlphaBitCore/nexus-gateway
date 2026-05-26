// Pgxmock-driven Run() coverage for ThingOfflineAlertsJob, supplementing
// the existing DB-backed test which skips without Postgres.

package health

import (
	"context"
	"errors"
	"testing"
	"time"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	"github.com/pashagolub/pgxmock/v4"
)

func TestThingOfflineAlerts_Run_RuleNotFound(t *testing.T) {
	loader := &fakeRuleLoader{err: alerting.ErrNotFound}
	j := &ThingOfflineAlertsJob{ruleLoader: loader, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestThingOfflineAlerts_Run_LoadError(t *testing.T) {
	sentinel := errors.New("load boom")
	loader := &fakeRuleLoader{err: sentinel}
	j := &ThingOfflineAlertsJob{ruleLoader: loader, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestThingOfflineAlerts_Run_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	loader := &fakeRuleLoader{rule: thingOfflineRule(300, nil)}

	old := time.Now().Add(-1 * time.Hour).UTC()
	displayName := "host-1"
	mock.ExpectQuery(`FROM thing`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type", "name", "last_seen_at"}).
			AddRow("thing-1", "agent", &displayName, old).
			AddRow("thing-2", "agent", (*string)(nil), old))
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(thingOfflineRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			AddRow("thing:thing-1").
			AddRow("thing:thing-recovered").
			AddRow("malformed-key"))

	raiser := &fakeRaiser{}
	j := &ThingOfflineAlertsJob{pool: mock, raiser: raiser, ruleLoader: loader, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(raiser.raises) != 2 {
		t.Errorf("raises = %d, want 2", len(raiser.raises))
	}
	if len(raiser.resolves) != 1 {
		t.Errorf("resolves = %d, want 1", len(raiser.resolves))
	}
}

func TestThingOfflineAlerts_Run_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	loader := &fakeRuleLoader{rule: thingOfflineRule(300, nil)}
	sentinel := errors.New("query boom")
	mock.ExpectQuery(`FROM thing`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(sentinel)

	j := &ThingOfflineAlertsJob{pool: mock, raiser: &fakeRaiser{}, ruleLoader: loader, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestThingOfflineAlerts_Run_ParamsError(t *testing.T) {
	rule := &alerting.AlertRule{
		ID:      thingOfflineRuleID,
		Enabled: true,
		Params:  map[string]any{}, // missing offlineAfterSec
	}
	loader := &fakeRuleLoader{rule: rule}
	j := &ThingOfflineAlertsJob{ruleLoader: loader, logger: testLogger()}
	if err := j.Run(context.Background()); err == nil {
		t.Fatalf("expected parse error")
	}
}

func TestThingOfflineAlerts_ResolveRecovered_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("resolve query boom")
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(thingOfflineRuleID).WillReturnError(sentinel)

	j := &ThingOfflineAlertsJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	if err := j.resolveRecovered(context.Background(), map[string]bool{}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}
