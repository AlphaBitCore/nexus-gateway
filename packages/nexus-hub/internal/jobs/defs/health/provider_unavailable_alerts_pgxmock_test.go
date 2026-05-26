// Pgxmock-driven test for ProviderUnavailableAlertsJob.resolveRecovered.

package health

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestProviderUnavailableAlerts_ResolveRecovered_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("query boom")
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(providerUnavailableRuleID).WillReturnError(sentinel)

	j := &ProviderUnavailableAlertsJob{
		pool:             mock,
		raiser:           &fakeRaiser{},
		logger:           testLogger(),
		unavailableSince: make(map[string]time.Time),
		recoveredSince:   make(map[string]time.Time),
	}
	if err := j.resolveRecovered(context.Background(), map[string]bool{}, 0, time.Now()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestProviderUnavailableAlerts_ResolveRecovered_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(providerUnavailableRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			AddRow("provider:still-fire").
			AddRow("provider:recovered").
			AddRow("malformed-key"))

	raiser := &fakeRaiser{}
	j := &ProviderUnavailableAlertsJob{
		pool:             mock,
		raiser:           raiser,
		logger:           testLogger(),
		unavailableSince: map[string]time.Time{"recovered": time.Now()},
		recoveredSince:   make(map[string]time.Time),
	}
	shouldFire := map[string]bool{"still-fire": true}
	now := time.Now()
	if err := j.resolveRecovered(context.Background(), shouldFire, 0, now); err != nil {
		t.Fatalf("resolveRecovered: %v", err)
	}
	if len(raiser.resolves) != 1 || raiser.resolves[0].TargetKey != "provider:recovered" {
		t.Errorf("resolves = %v", raiser.resolves)
	}
}

func TestProviderUnavailableAlerts_ResolveRecovered_DebounceDefersResolve(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(providerUnavailableRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			AddRow("provider:recovering"))

	raiser := &fakeRaiser{}
	j := &ProviderUnavailableAlertsJob{
		pool:             mock,
		raiser:           raiser,
		logger:           testLogger(),
		unavailableSince: make(map[string]time.Time),
		recoveredSince:   make(map[string]time.Time),
	}
	// recoverySec = 60; now=just started recovering → defer resolve.
	if err := j.resolveRecovered(context.Background(), map[string]bool{}, 60, time.Now()); err != nil {
		t.Fatalf("resolveRecovered: %v", err)
	}
	if len(raiser.resolves) != 0 {
		t.Errorf("expected no resolves yet, got %v", raiser.resolves)
	}
}

func TestProviderUnavailableAlerts_ResolveRecovered_RaiseFailure(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(providerUnavailableRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			AddRow("provider:rec"))

	raiser := &errRaiser{resolveErr: errors.New("resolve boom")}
	j := &ProviderUnavailableAlertsJob{
		pool:             mock,
		raiser:           raiser,
		logger:           testLogger(),
		unavailableSince: make(map[string]time.Time),
		recoveredSince:   make(map[string]time.Time),
	}
	// recoverySec=0 → resolve immediately; raiser fails → warn, no err.
	if err := j.resolveRecovered(context.Background(), map[string]bool{}, 0, time.Now()); err != nil {
		t.Errorf("expected nil err, got %v", err)
	}
}
