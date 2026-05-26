// Second batch of extra pgxmock tests covering the remaining statement gaps
// after health_extra_test.go raised coverage from 79.9% to 94.4%.
//
// Targeted gaps:
//   - rows.Err() and Scan() paths in the inner loops of Run/resolveRecovered
//   - credential_stale_alerts: int / int64 staleAfterDays branches
//   - credential_reliability_alerts: raise error warn path
//   - thing_offline_alerts: raise error accumulation + resolve-recovered error
//   - provider_unavailable_alerts: scan errors + resolveRecovered err path
//   - cache_quality_monitor: marshal error + rows.Err in revertToDryRun

package health

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

// credential_stale_alerts.go: staleAfterDays int / int64 branches

// TestCredentialStaleAlerts_Run_StaleAfterDaysInt covers the int type branch
// in staleAfterDays extraction (line 76-77).
func TestCredentialStaleAlerts_Run_StaleAfterDaysInt(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	rule := &alerting.AlertRule{
		ID:      credStaleRuleID,
		Enabled: true,
		Params:  map[string]any{"staleAfterDays": int(14)},
	}
	// Returns zero stale credentials → no raise.
	mock.ExpectQuery(`FROM "Credential"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "lastSuccessAt", "lastUsedAt"}))
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(credStaleRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	j := &CredentialStaleAlertsJob{pool: mock, raiser: &fakeRaiser{}, ruleLoader: &fakeRuleLoader{rule: rule}, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestCredentialStaleAlerts_Run_StaleAfterDaysInt64 covers the int64 type
// branch in staleAfterDays extraction (line 78-80).
func TestCredentialStaleAlerts_Run_StaleAfterDaysInt64(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	rule := &alerting.AlertRule{
		ID:      credStaleRuleID,
		Enabled: true,
		Params:  map[string]any{"staleAfterDays": int64(7)},
	}
	mock.ExpectQuery(`FROM "Credential"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "lastSuccessAt", "lastUsedAt"}))
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(credStaleRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	j := &CredentialStaleAlertsJob{pool: mock, raiser: &fakeRaiser{}, ruleLoader: &fakeRuleLoader{rule: rule}, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestCredentialStaleAlerts_Run_ScanError covers the Scan error in the
// credential row loop (line 109-111).
func TestCredentialStaleAlerts_Run_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Return too few columns for a 4-column Scan → scan error.
	mock.ExpectQuery(`FROM "Credential"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("cred-1"))

	j := &CredentialStaleAlertsJob{
		pool:       mock,
		raiser:     &fakeRaiser{},
		ruleLoader: &fakeRuleLoader{rule: makeStaleRule(7, true)},
		logger:     testLogger(),
	}
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected scan error")
	}
}

// TestCredentialStaleAlerts_Run_RowsErrPropagates covers the rows.Err() path
// after the scan loop (line 114-116).
func TestCredentialStaleAlerts_Run_RowsErrPropagates(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("rows iter err")
	mock.ExpectQuery(`FROM "Credential"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "lastSuccessAt", "lastUsedAt"}).
			AddRow("c1", "cred1", (*time.Time)(nil), (*time.Time)(nil)).
			RowError(0, sentinel))

	j := &CredentialStaleAlertsJob{
		pool:       mock,
		raiser:     &fakeRaiser{},
		ruleLoader: &fakeRuleLoader{rule: makeStaleRule(7, true)},
		logger:     testLogger(),
	}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// TestCredentialStaleAlerts_ResolveRecovered_RowsErrPropagates covers the
// rows.Err() in resolveRecovered (line 173-175).
func TestCredentialStaleAlerts_ResolveRecovered_RowsErrPropagates(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("iter err")
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(credStaleRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			AddRow("credential:c1").RowError(0, sentinel))

	j := &CredentialStaleAlertsJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	if err := j.resolveRecovered(context.Background(), map[string]bool{}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// credential_reliability_alerts.go: raise — error warn path

// TestCredentialReliabilityAlerts_Run_RaiseErrorWarns verifies that when
// raiser.Raise fails, the job logs a warning and continues (no Run error).
func TestCredentialReliabilityAlerts_Run_RaiseErrorWarns(t *testing.T) {
	loader := &multiRuleLoader{rules: map[string]*alerting.AlertRule{
		credCircuitOpenRuleID: enabledReliabilityRule(credCircuitOpenRuleID, alerting.SeverityHigh),
	}}
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	openTime := time.Now().Add(-5 * time.Minute).UTC()
	openErr := "auth_fail"
	mock.ExpectQuery(`FROM   "Credential"`).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "name", "circuitState", "circuitReason", "circuitOpenedAt",
			"healthStatus", "healthDominantError", "healthTrend", "healthStatusChangedAt",
		}).AddRow("cred-err", "cred-err-name", "open", &openErr, &openTime,
			"healthy", (*string)(nil), (*string)(nil), (*time.Time)(nil)))

	// resolveRecovered for circuit_open: empty.
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(credCircuitOpenRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	raiser := &errRaiser{raiseErr: errors.New("raise boom")}
	j := &CredentialReliabilityAlertsJob{
		pool:       mock,
		raiser:     raiser,
		ruleLoader: loader,
		logger:     testLogger(),
	}
	// raise error is logged as Warn — Run itself must succeed.
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run must not return raise warn error; got %v", err)
	}
}

// TestCredentialReliabilityAlerts_Run_ResolveRecoveredWarn covers the
// resolveRecovered warn path in Run (line 141-143): when resolveRecovered
// returns an error it's logged but Run still returns nil.
func TestCredentialReliabilityAlerts_Run_ResolveRecoveredWarn(t *testing.T) {
	loader := &multiRuleLoader{rules: map[string]*alerting.AlertRule{
		credCircuitOpenRuleID: enabledReliabilityRule(credCircuitOpenRuleID, alerting.SeverityHigh),
	}}
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// snapshot returns no rows → no raises.
	mock.ExpectQuery(`FROM   "Credential"`).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "name", "circuitState", "circuitReason", "circuitOpenedAt",
			"healthStatus", "healthDominantError", "healthTrend", "healthStatusChangedAt",
		}))

	// resolveRecovered query errors → logged as Warn in Run.
	sentinel := errors.New("resolve boom")
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(credCircuitOpenRuleID).
		WillReturnError(sentinel)

	j := &CredentialReliabilityAlertsJob{
		pool:       mock,
		raiser:     &fakeRaiser{},
		ruleLoader: loader,
		logger:     testLogger(),
	}
	// resolveRecovered error is just logged → Run must still return nil.
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run must not return resolveRecovered error; got %v", err)
	}
}

// thing_offline_alerts.go: Run — raise error and resolve-recovered err paths

// TestThingOfflineAlerts_Run_RaiseError covers the raise error accumulation
// path (line 160-162) — raise fails for a stale thing.
func TestThingOfflineAlerts_Run_RaiseError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	loader := &fakeRuleLoader{rule: thingOfflineRule(300, nil)}
	old := time.Now().Add(-1 * time.Hour).UTC()
	hostname := "host-err"
	mock.ExpectQuery(`FROM thing`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type", "name", "last_seen_at"}).
			AddRow("thing-raise-err", "agent", &hostname, old))
	// resolveRecovered: empty.
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(thingOfflineRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	sentinel := errors.New("raise boom")
	j := &ThingOfflineAlertsJob{
		pool:       mock,
		raiser:     &errRaiser{raiseErr: sentinel},
		ruleLoader: loader,
		logger:     testLogger(),
	}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want raise sentinel", err)
	}
}

// TestThingOfflineAlerts_Run_ScanError covers the Scan error in the thing
// row loop (line 122-124).
func TestThingOfflineAlerts_Run_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	loader := &fakeRuleLoader{rule: thingOfflineRule(300, nil)}
	// Return 2-column rows for a 4-column Scan.
	mock.ExpectQuery(`FROM thing`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type"}).AddRow("thing-1", "agent"))

	j := &ThingOfflineAlertsJob{
		pool:       mock,
		raiser:     &fakeRaiser{},
		ruleLoader: loader,
		logger:     testLogger(),
	}
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected scan error")
	}
}

// TestThingOfflineAlerts_Run_RowsErrPropagates covers the rows.Err() path in
// the thing scan loop (line 127-129).
func TestThingOfflineAlerts_Run_RowsErrPropagates(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	loader := &fakeRuleLoader{rule: thingOfflineRule(300, nil)}
	sentinel := errors.New("iter err")
	old := time.Now().Add(-1 * time.Hour).UTC()
	hostname := "h1"
	mock.ExpectQuery(`FROM thing`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type", "name", "last_seen_at"}).
			AddRow("thing-1", "agent", &hostname, old).
			RowError(0, sentinel))

	j := &ThingOfflineAlertsJob{
		pool:       mock,
		raiser:     &fakeRaiser{},
		ruleLoader: loader,
		logger:     testLogger(),
	}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// TestThingOfflineAlerts_Run_ResolveRecoveredErr covers the resolve-recovered
// error accumulation path (line 168-170).
func TestThingOfflineAlerts_Run_ResolveRecoveredErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	loader := &fakeRuleLoader{rule: thingOfflineRule(300, nil)}
	// No stale things.
	mock.ExpectQuery(`FROM thing`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type", "name", "last_seen_at"}))
	// resolveRecovered query fails.
	sentinel := errors.New("resolve boom")
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(thingOfflineRuleID).
		WillReturnError(sentinel)

	j := &ThingOfflineAlertsJob{
		pool:       mock,
		raiser:     &fakeRaiser{},
		ruleLoader: loader,
		logger:     testLogger(),
	}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// TestThingOfflineAlerts_ResolveRecovered_RowsErr covers the rows.Err() path
// in resolveRecovered (line 202-204).
func TestThingOfflineAlerts_ResolveRecovered_RowsErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("iter err")
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(thingOfflineRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			AddRow("thing:x").RowError(0, sentinel))

	j := &ThingOfflineAlertsJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	if err := j.resolveRecovered(context.Background(), map[string]bool{}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// provider_unavailable_alerts.go: Run — scan errors + resolveRecovered err

// TestProviderUnavailableAlerts_Run_ScanError covers the Scan error in the
// unavailable providers loop (line 133-135).
func TestProviderUnavailableAlerts_Run_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Return too few columns for a 6-column Scan.
	mock.ExpectQuery(`FROM "ProviderHealth"`).
		WillReturnRows(pgxmock.NewRows([]string{"providerId"}).AddRow("prov-1"))

	j := &ProviderUnavailableAlertsJob{
		pool:             mock,
		raiser:           &fakeRaiser{},
		ruleLoader:       &fakeRuleLoader{rule: providerUnavailableRule(0, 0)},
		logger:           testLogger(),
		unavailableSince: make(map[string]time.Time),
		recoveredSince:   make(map[string]time.Time),
	}
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected scan error")
	}
}

// TestProviderUnavailableAlerts_Run_RowsErrPropagates covers the rows.Err()
// path in the unavailable providers loop (line 139-141).
func TestProviderUnavailableAlerts_Run_RowsErrPropagates(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("iter err")
	lastErr := time.Now().UTC()
	mock.ExpectQuery(`FROM "ProviderHealth"`).
		WillReturnRows(pgxmock.NewRows([]string{
			"providerId", "provider", "display_name",
			"rollingErrorRate", "lastErrorAt", "updatedAt",
		}).AddRow("prov-1", "openai", "OpenAI", 0.9, &lastErr, lastErr).
			RowError(0, sentinel))

	j := &ProviderUnavailableAlertsJob{
		pool:             mock,
		raiser:           &fakeRaiser{},
		ruleLoader:       &fakeRuleLoader{rule: providerUnavailableRule(0, 0)},
		logger:           testLogger(),
		unavailableSince: make(map[string]time.Time),
		recoveredSince:   make(map[string]time.Time),
	}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// TestProviderUnavailableAlerts_Run_ResolveRecoveredErrAccumulates covers the
// resolve-recovered error accumulation path (line 197-199): when resolveRecovered
// returns an error it's appended to errs and returned via errors.Join.
func TestProviderUnavailableAlerts_Run_ResolveRecoveredErrAccumulates(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// No unavailable providers.
	mock.ExpectQuery(`FROM "ProviderHealth"`).
		WillReturnRows(pgxmock.NewRows([]string{
			"providerId", "provider", "display_name",
			"rollingErrorRate", "lastErrorAt", "updatedAt",
		}))
	// resolveRecovered query fails.
	sentinel := errors.New("resolve boom")
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(providerUnavailableRuleID).
		WillReturnError(sentinel)

	j := &ProviderUnavailableAlertsJob{
		pool:             mock,
		raiser:           &fakeRaiser{},
		ruleLoader:       &fakeRuleLoader{rule: providerUnavailableRule(0, 0)},
		logger:           testLogger(),
		unavailableSince: make(map[string]time.Time),
		recoveredSince:   make(map[string]time.Time),
	}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// TestProviderUnavailableAlerts_ResolveRecovered_ScanErr covers the scan
// error in the resolveRecovered targetKey loop.
func TestProviderUnavailableAlerts_ResolveRecovered_ScanErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(providerUnavailableRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey", "extra"}).
			AddRow("provider:x", "boom"))

	j := &ProviderUnavailableAlertsJob{
		pool:             mock,
		raiser:           &fakeRaiser{},
		logger:           testLogger(),
		unavailableSince: make(map[string]time.Time),
		recoveredSince:   make(map[string]time.Time),
	}
	if err := j.resolveRecovered(context.Background(), map[string]bool{}, 0, time.Now()); err == nil {
		t.Fatal("expected scan error")
	}
}

// TestProviderUnavailableAlerts_ResolveRecovered_RowsErr covers the rows.Err()
// path in resolveRecovered (line 232-234).
func TestProviderUnavailableAlerts_ResolveRecovered_RowsErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("iter err")
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(providerUnavailableRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			AddRow("provider:x").RowError(0, sentinel))

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

// cache_quality_monitor.go: revertToDryRun — marshal error + rows.Err

// TestCacheQualityMonitor_RevertToDryRun_MarshalError covers the Marshal
// error path (line 166-168) — a rule that mutates cfg but json.Marshal fails.
// We can't easily cause json.Marshal to fail on a map[string]any, so instead
// we test the rows.Err() path for the scan loop.

// TestCacheQualityMonitor_RevertToDryRun_RowsErrAfterRows covers the
// rows.Err() path (line 173-175) after the scan loop.
func TestCacheQualityMonitor_RevertToDryRun_RowsErrAfterRows(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("iter err")
	mock.ExpectQuery(`FROM cache_adapter_config`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}).
			AddRow("openai", []byte(`{"rules":{"r1":{"enabled":false}}}`)).
			RowError(0, sentinel))

	j := &CacheQualityMonitorJob{pool: mock, logger: testLogger()}
	if err := j.revertToDryRun(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// TestCacheQualityMonitor_RevertToDryRun_RuleNotMap covers the ruleRaw not
// being a map[string]any (line 151-153) — skip the rule entry.
func TestCacheQualityMonitor_RevertToDryRun_RuleNotMap(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// rules has a string value instead of a map.
	mock.ExpectQuery(`FROM cache_adapter_config`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}).
			AddRow("openai", []byte(`{"rules":{"r1":"not-a-map"}}`)))

	j := &CacheQualityMonitorJob{pool: mock, logger: testLogger()}
	// No UPDATE expected — all rules are skipped.
	if err := j.revertToDryRun(context.Background()); err != nil {
		t.Fatalf("revertToDryRun: %v", err)
	}
}

// TestCacheQualityMonitor_Run_BaselineAboveThreshold_NoPending covers the
// case where revertToDryRun is called but no rows have enabled=true rules —
// exercises the "no enabled rules" info-log path inside revertToDryRun.
// This is distinct from TestCacheQualityMonitor_RevertToDryRun_NoEnabledRules
// because here we also exercise the Run→revertToDryRun call chain with a
// real query path.
func TestCacheQualityMonitor_Run_BaselineFloorNoPending(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Regression: totalNorm=50 errorNorm=30 (60%), totalAll=0 → baseline→0.01;
	// 60% > 0.01*3=3% → trigger revertToDryRun.
	expectStats(mock, 50, 30, 0, 0)
	// revertToDryRun: one row with only disabled rules → no UPDATE.
	mock.ExpectQuery(`FROM cache_adapter_config`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}).
			AddRow("gemini", []byte(`{"rules":{"r1":{"enabled":false}}}`)))

	j := &CacheQualityMonitorJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
