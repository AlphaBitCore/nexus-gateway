// Extra pgxmock-driven tests for the health package, targeting the
// statement-level gaps left after the first test pass:
//
//   - provider_unavailable_alerts.go:Run (7.5%) — full Run path via pgxmock
//   - parseProviderUnavailableParams (82.4%) — wrong-type recoverySec, negative values
//   - parseWarnDays (73.3%) — []int branch, int/int64 elements in []any
//   - warnDaysBand (75.0%) — daysLeft > all thresholds → 0
//   - credential_reliability_alerts.go:resolveRecovered (42.9%) — rows with data
//   - credential_reliability_alerts.go:snapshot (90.9%) — scan error branch
//   - cache_quality_monitor.go:revertToDryRun (83.3%) — scan error, no-rules branch
//   - thing/credStale/agentCert resolveRecovered scan-error branches
//   - agent_cert_expiration_alerts.go:Run (87.2%) — warnDays empty / band=0 paths

package health

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

// provider_unavailable_alerts.go: Run — full pgxmock path

// TestProviderUnavailableAlerts_Run_NoUnavailable tests Run when the DB
// returns zero unavailable providers — resolveRecovered query is still issued
// but returns no firing alerts. Verifies no Raise, no Resolve.
func TestProviderUnavailableAlerts_Run_NoUnavailable(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Unavailable providers query returns zero rows.
	mock.ExpectQuery(`FROM "ProviderHealth"`).
		WillReturnRows(pgxmock.NewRows([]string{
			"providerId", "provider", "display_name",
			"rollingErrorRate", "lastErrorAt", "updatedAt",
		}))
	// resolveRecovered query also returns zero rows.
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(providerUnavailableRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	raiser := &fakeRaiser{}
	j := &ProviderUnavailableAlertsJob{
		pool:             mock,
		raiser:           raiser,
		ruleLoader:       &fakeRuleLoader{rule: providerUnavailableRule(0, 0)},
		logger:           testLogger(),
		unavailableSince: make(map[string]time.Time),
		recoveredSince:   make(map[string]time.Time),
	}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if mock.ExpectationsWereMet() != nil {
		t.Errorf("mock expectations: %v", mock.ExpectationsWereMet())
	}
	if raiser.raiseCount() != 0 {
		t.Errorf("raises = %d, want 0", raiser.raiseCount())
	}
}

// TestProviderUnavailableAlerts_Run_UnavailableWithLastErrorAt tests the
// lastErrorAt branch in the details map (non-nil pointer).
func TestProviderUnavailableAlerts_Run_UnavailableWithLastErrorAt(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	lastErr := time.Now().Add(-5 * time.Minute).UTC()
	mock.ExpectQuery(`FROM "ProviderHealth"`).
		WillReturnRows(pgxmock.NewRows([]string{
			"providerId", "provider", "display_name",
			"rollingErrorRate", "lastErrorAt", "updatedAt",
		}).AddRow("prov-1", "openai", "OpenAI", 0.95, &lastErr, time.Now().UTC()))
	// resolveRecovered: no firing alerts.
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(providerUnavailableRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	raiser := &fakeRaiser{}
	j := &ProviderUnavailableAlertsJob{
		pool:             mock,
		raiser:           raiser,
		ruleLoader:       &fakeRuleLoader{rule: providerUnavailableRule(0, 0)},
		logger:           testLogger(),
		unavailableSince: make(map[string]time.Time),
		recoveredSince:   make(map[string]time.Time),
	}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if raiser.raiseCount() != 1 {
		t.Errorf("raises = %d, want 1", raiser.raiseCount())
	}
}

// TestProviderUnavailableAlerts_Run_UnavailableNoLastErrorAt tests the
// nil lastErrorAt branch (details["lastErrorAt"] key is skipped).
func TestProviderUnavailableAlerts_Run_UnavailableNoLastErrorAt(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM "ProviderHealth"`).
		WillReturnRows(pgxmock.NewRows([]string{
			"providerId", "provider", "display_name",
			"rollingErrorRate", "lastErrorAt", "updatedAt",
		}).AddRow("prov-2", "anthropic", "Anthropic", 1.0, (*time.Time)(nil), time.Now().UTC()))
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(providerUnavailableRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	raiser := &fakeRaiser{}
	j := &ProviderUnavailableAlertsJob{
		pool:             mock,
		raiser:           raiser,
		ruleLoader:       &fakeRuleLoader{rule: providerUnavailableRule(0, 0)},
		logger:           testLogger(),
		unavailableSince: make(map[string]time.Time),
		recoveredSince:   make(map[string]time.Time),
	}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if raiser.raiseCount() != 1 {
		t.Errorf("raises = %d, want 1", raiser.raiseCount())
	}
}

// TestProviderUnavailableAlerts_Run_QueryError tests the providers query error
// path (returns immediately with wrapped error).
func TestProviderUnavailableAlerts_Run_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("db boom")
	mock.ExpectQuery(`FROM "ProviderHealth"`).WillReturnError(sentinel)

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

// TestProviderUnavailableAlerts_Run_RuleNotFound tests the ErrNotFound
// branch — Run returns nil without touching the DB.
func TestProviderUnavailableAlerts_Run_RuleNotFound(t *testing.T) {
	j := &ProviderUnavailableAlertsJob{
		ruleLoader:       &fakeRuleLoader{err: alerting.ErrNotFound},
		logger:           testLogger(),
		unavailableSince: make(map[string]time.Time),
		recoveredSince:   make(map[string]time.Time),
	}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestProviderUnavailableAlerts_Run_LoadError tests a non-ErrNotFound rule
// load error propagating through Run.
func TestProviderUnavailableAlerts_Run_LoadError(t *testing.T) {
	sentinel := errors.New("rule load boom")
	j := &ProviderUnavailableAlertsJob{
		ruleLoader:       &fakeRuleLoader{err: sentinel},
		logger:           testLogger(),
		unavailableSince: make(map[string]time.Time),
		recoveredSince:   make(map[string]time.Time),
	}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// TestProviderUnavailableAlerts_Run_ParseParamsError tests the param parse
// error path in Run.
func TestProviderUnavailableAlerts_Run_ParseParamsError(t *testing.T) {
	rule := &alerting.AlertRule{
		ID:      providerUnavailableRuleID,
		Enabled: true,
		// missing both params
		Params: map[string]any{},
	}
	j := &ProviderUnavailableAlertsJob{
		ruleLoader:       &fakeRuleLoader{rule: rule},
		logger:           testLogger(),
		unavailableSince: make(map[string]time.Time),
		recoveredSince:   make(map[string]time.Time),
	}
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected parse error")
	}
}

// TestProviderUnavailableAlerts_Run_MinDownSecDefers verifies that a provider
// with minDownSec > 0 is deferred on the first tick (not yet raised).
func TestProviderUnavailableAlerts_Run_MinDownSecDefers(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM "ProviderHealth"`).
		WillReturnRows(pgxmock.NewRows([]string{
			"providerId", "provider", "display_name",
			"rollingErrorRate", "lastErrorAt", "updatedAt",
		}).AddRow("prov-defer", "openai", "OpenAI", 0.9, (*time.Time)(nil), time.Now().UTC()))
	// resolveRecovered returns no firing alerts.
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(providerUnavailableRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	raiser := &fakeRaiser{}
	// minDownSec=3600 (1h) → first-seen=now → 0 elapsed < 3600 → skip raise.
	j := &ProviderUnavailableAlertsJob{
		pool:             mock,
		raiser:           raiser,
		ruleLoader:       &fakeRuleLoader{rule: providerUnavailableRule(3600, 0)},
		logger:           testLogger(),
		unavailableSince: make(map[string]time.Time),
		recoveredSince:   make(map[string]time.Time),
	}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// First tick with minDownSec=3600 should NOT raise (debounced).
	if raiser.raiseCount() != 0 {
		t.Errorf("raises = %d, want 0 (debounced)", raiser.raiseCount())
	}
}

// TestProviderUnavailableAlerts_Run_MinDownSecFires verifies that a provider
// that has been in unavailableSince long enough gets raised.
func TestProviderUnavailableAlerts_Run_MinDownSecFires(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM "ProviderHealth"`).
		WillReturnRows(pgxmock.NewRows([]string{
			"providerId", "provider", "display_name",
			"rollingErrorRate", "lastErrorAt", "updatedAt",
		}).AddRow("prov-long", "openai", "OpenAI", 0.9, (*time.Time)(nil), time.Now().UTC()))
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(providerUnavailableRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	raiser := &fakeRaiser{}
	// Pre-seed unavailableSince to 2 hours ago so minDownSec=60 is satisfied.
	twoHoursAgo := time.Now().UTC().Add(-2 * time.Hour)
	j := &ProviderUnavailableAlertsJob{
		pool:             mock,
		raiser:           raiser,
		ruleLoader:       &fakeRuleLoader{rule: providerUnavailableRule(60, 0)},
		logger:           testLogger(),
		unavailableSince: map[string]time.Time{"prov-long": twoHoursAgo},
		recoveredSince:   make(map[string]time.Time),
	}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if raiser.raiseCount() != 1 {
		t.Errorf("raises = %d, want 1 (minDownSec elapsed)", raiser.raiseCount())
	}
}

// TestProviderUnavailableAlerts_Run_RaiseError verifies that a raiser error
// is accumulated and returned from Run (via errors.Join).
func TestProviderUnavailableAlerts_Run_RaiseError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM "ProviderHealth"`).
		WillReturnRows(pgxmock.NewRows([]string{
			"providerId", "provider", "display_name",
			"rollingErrorRate", "lastErrorAt", "updatedAt",
		}).AddRow("prov-err", "openai", "OpenAI", 1.0, (*time.Time)(nil), time.Now().UTC()))
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(providerUnavailableRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	sentinel := errors.New("raise boom")
	j := &ProviderUnavailableAlertsJob{
		pool:             mock,
		raiser:           &errRaiser{raiseErr: sentinel},
		ruleLoader:       &fakeRuleLoader{rule: providerUnavailableRule(0, 0)},
		logger:           testLogger(),
		unavailableSince: make(map[string]time.Time),
		recoveredSince:   make(map[string]time.Time),
	}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// parseProviderUnavailableParams — remaining branches

// TestParseProviderUnavailableParams_WrongTypeRecoverySec covers the wrong
// type assertion for recoverySec (line 299-301).
func TestParseProviderUnavailableParams_WrongTypeRecoverySec(t *testing.T) {
	_, _, err := parseProviderUnavailableParams(map[string]any{
		"minDownSec":  float64(30),
		"recoverySec": "not-a-number",
	})
	if err == nil {
		t.Error("expected error for non-numeric recoverySec")
	}
}

// TestParseProviderUnavailableParams_NegativeMinDownSec covers the negative
// minDownSec validation (line 292-294).
func TestParseProviderUnavailableParams_NegativeMinDownSec(t *testing.T) {
	_, _, err := parseProviderUnavailableParams(map[string]any{
		"minDownSec":  float64(-5),
		"recoverySec": float64(30),
	})
	if err == nil {
		t.Error("expected error for negative minDownSec")
	}
}

// TestParseProviderUnavailableParams_NegativeRecoverySec covers the negative
// recoverySec validation (line 303-305).
func TestParseProviderUnavailableParams_NegativeRecoverySec(t *testing.T) {
	_, _, err := parseProviderUnavailableParams(map[string]any{
		"minDownSec":  float64(30),
		"recoverySec": float64(-1),
	})
	if err == nil {
		t.Error("expected error for negative recoverySec")
	}
}

// parseWarnDays — []int branch and int/int64 element types

// TestParseWarnDays_SliceInt covers the []int type-switch branch (line 209).
func TestParseWarnDays_SliceInt(t *testing.T) {
	out, err := parseWarnDays(map[string]any{
		"warnDays": []int{14, 7, 30},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Result must be sorted ascending.
	if len(out) != 3 || out[0] != 7 || out[1] != 14 || out[2] != 30 {
		t.Errorf("warnDays = %v, want [7 14 30]", out)
	}
}

// TestParseWarnDays_SliceAnyInt covers the int element branch inside []any.
func TestParseWarnDays_SliceAnyInt(t *testing.T) {
	out, err := parseWarnDays(map[string]any{
		"warnDays": []any{int(7), int(30)},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 || out[0] != 7 || out[1] != 30 {
		t.Errorf("warnDays = %v, want [7 30]", out)
	}
}

// TestParseWarnDays_SliceAnyInt64 covers the int64 element branch inside []any.
func TestParseWarnDays_SliceAnyInt64(t *testing.T) {
	out, err := parseWarnDays(map[string]any{
		"warnDays": []any{int64(1), int64(14)},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 || out[0] != 1 || out[1] != 14 {
		t.Errorf("warnDays = %v, want [1 14]", out)
	}
}

// TestParseWarnDays_SliceAnyMixedIgnoresUnknown covers mixed []any where
// unknown element types (string) are silently skipped — the output still has
// the valid entries from known numeric types.
func TestParseWarnDays_SliceAnyMixedIgnoresUnknown(t *testing.T) {
	out, err := parseWarnDays(map[string]any{
		"warnDays": []any{float64(7), "ignored"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 || out[0] != 7 {
		t.Errorf("warnDays = %v, want [7]", out)
	}
}

// TestParseWarnDays_EmptySliceError covers the empty-output error when []any
// contains no recognisable values.
func TestParseWarnDays_EmptySliceError(t *testing.T) {
	_, err := parseWarnDays(map[string]any{
		"warnDays": []any{"not-a-number"},
	})
	if err == nil {
		t.Error("expected error for empty output slice")
	}
}

// warnDaysBand — daysLeft > every threshold → returns 0

// TestWarnDaysBand_OutsideWindow covers the case where daysLeft > every
// warnDays entry — warnDaysBand must return 0 (should not fire).
func TestWarnDaysBand_OutsideWindow(t *testing.T) {
	warnDays := []int{1, 7, 30}
	// 60 days left — outside the 30-day warn window.
	if got := warnDaysBand(warnDays, 60); got != 0 {
		t.Errorf("warnDaysBand(60) = %d, want 0 (outside window)", got)
	}
}

// TestWarnDaysBand_ExactBoundary covers daysLeft == the smallest threshold.
func TestWarnDaysBand_ExactBoundary(t *testing.T) {
	warnDays := []int{1, 7, 30}
	if got := warnDaysBand(warnDays, 1); got != 1 {
		t.Errorf("warnDaysBand(1) = %d, want 1", got)
	}
	if got := warnDaysBand(warnDays, 7); got != 7 {
		t.Errorf("warnDaysBand(7) = %d, want 7", got)
	}
}

// credential_reliability_alerts.go: resolveRecovered — rows with data

// TestCredentialReliabilityAlerts_ResolveRecovered_HappyPath drives
// resolveRecovered with rows: one still-firing credential and one recovered.
// Verifies Resolve is called only for the recovered credential.
func TestCredentialReliabilityAlerts_ResolveRecovered_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(credCircuitOpenRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			AddRow("credential:still-open").
			AddRow("credential:now-closed").
			AddRow("bad-format-key"))

	raiser := &fakeRaiser{}
	j := &CredentialReliabilityAlertsJob{pool: mock, raiser: raiser, logger: testLogger()}

	// still-open is currently firing → should NOT be resolved.
	currentlyFiring := map[string]bool{"still-open": true}
	if err := j.resolveRecovered(context.Background(), credCircuitOpenRuleID, currentlyFiring); err != nil {
		t.Fatalf("resolveRecovered: %v", err)
	}
	if len(raiser.resolves) != 1 {
		t.Errorf("resolves = %d, want 1 (now-closed)", len(raiser.resolves))
	}
	if raiser.resolves[0].TargetKey != "credential:now-closed" {
		t.Errorf("resolve targetKey = %q", raiser.resolves[0].TargetKey)
	}
	if raiser.resolves[0].Reason != "recovered" {
		t.Errorf("resolve reason = %q, want recovered", raiser.resolves[0].Reason)
	}
}

// TestCredentialReliabilityAlerts_ResolveRecovered_RowsErrPropagates covers
// the rows.Err() path (line 246-248): pgxmock closes rows with an error.
func TestCredentialReliabilityAlerts_ResolveRecovered_RowsErrPropagates(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("rows iter err")
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(credCircuitOpenRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			AddRow("credential:x").RowError(0, sentinel))

	j := &CredentialReliabilityAlertsJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	if err := j.resolveRecovered(context.Background(), credCircuitOpenRuleID, map[string]bool{}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// TestCredentialReliabilityAlerts_ResolveRecovered_ResolveErrorWarns verifies
// that a Resolve failure is logged (warn) and does NOT propagate as an error.
func TestCredentialReliabilityAlerts_ResolveRecovered_ResolveErrorWarns(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(credCircuitOpenRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			AddRow("credential:gone"))

	raiser := &errRaiser{resolveErr: errors.New("resolve boom")}
	j := &CredentialReliabilityAlertsJob{pool: mock, raiser: raiser, logger: testLogger()}

	// gone is not in currentlyFiring → should call Resolve → error is just logged.
	if err := j.resolveRecovered(context.Background(), credCircuitOpenRuleID, map[string]bool{}); err != nil {
		t.Fatalf("resolveRecovered must not return resolve error; got %v", err)
	}
}

// credential_reliability_alerts.go: snapshot — scan error branch

// TestCredentialReliabilityAlerts_Snapshot_ScanError covers the scan error
// path in snapshot(): pgxmock returns a row with wrong column count.
func TestCredentialReliabilityAlerts_Snapshot_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Return a row with only 1 column instead of the expected 9 — causes Scan error.
	mock.ExpectQuery(`FROM   "Credential"`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("cred-1"))

	j := &CredentialReliabilityAlertsJob{pool: mock, logger: testLogger()}
	_, err := j.snapshot(context.Background())
	if err == nil {
		t.Fatal("expected scan error")
	}
}

// cache_quality_monitor.go: revertToDryRun — scan error branch

// TestCacheQualityMonitor_RevertToDryRun_ScanError covers the scan error
// branch in revertToDryRun (line 132-135).
func TestCacheQualityMonitor_RevertToDryRun_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Return one column instead of two — causes Scan to fail.
	mock.ExpectQuery(`FROM cache_adapter_config`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type"}).AddRow("openai"))

	j := &CacheQualityMonitorJob{pool: mock, logger: testLogger()}
	if err := j.revertToDryRun(context.Background()); err == nil {
		t.Fatal("expected scan error")
	}
}

// TestCacheQualityMonitor_RevertToDryRun_NoRulesKey covers the cfg["rules"]
// absent branch (line 141-143) — row exists but config JSON has no "rules" key.
func TestCacheQualityMonitor_RevertToDryRun_NoRulesKey(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM cache_adapter_config`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}).
			AddRow("openai", []byte(`{"other_key": true}`)))

	j := &CacheQualityMonitorJob{pool: mock, logger: testLogger()}
	if err := j.revertToDryRun(context.Background()); err != nil {
		t.Fatalf("revertToDryRun: %v", err)
	}
}

// TestCacheQualityMonitor_RevertToDryRun_RulesNotMap covers the wrong type
// for cfg["rules"] (line 144-146) — rules is an array instead of a map.
func TestCacheQualityMonitor_RevertToDryRun_RulesNotMap(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM cache_adapter_config`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}).
			AddRow("openai", []byte(`{"rules": ["not","a","map"]}`)))

	j := &CacheQualityMonitorJob{pool: mock, logger: testLogger()}
	if err := j.revertToDryRun(context.Background()); err != nil {
		t.Fatalf("revertToDryRun: %v", err)
	}
}

// TestCacheQualityMonitor_Run_BaselineZeroFloor covers the branch where
// totalAll=0 → baselineErrorRate stays 0, then gets floored to 0.01.
func TestCacheQualityMonitor_Run_BaselineZeroFloor(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// totalNorm=25 (>20), errorNorm=0; totalAll=0 → baselineErrorRate=0→0.01;
	// normErrorRate=0 ≤ 0.01*3 → no regression.
	expectStats(mock, 25, 0, 0, 0)

	j := &CacheQualityMonitorJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// thing_offline_alerts.go resolveRecovered — scan error + rows.Err branches

// TestThingOfflineAlerts_ResolveRecovered_ScanError covers the scan error
// returned when the targetKey column returns an unexpected type.
func TestThingOfflineAlerts_ResolveRecovered_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// pgxmock only yields scan error via row count mismatch or column type.
	// Return two columns for a one-column Scan — triggers error.
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(thingOfflineRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey", "extra"}).AddRow("thing:x", "boom"))

	j := &ThingOfflineAlertsJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	if err := j.resolveRecovered(context.Background(), map[string]bool{}); err == nil {
		t.Fatal("expected scan error")
	}
}

// TestThingOfflineAlerts_ResolveRecovered_ResolveErrorWarns confirms Resolve
// failure is logged but not returned from resolveRecovered.
func TestThingOfflineAlerts_ResolveRecovered_ResolveErrorWarns(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(thingOfflineRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			AddRow("thing:gone-thing"))

	raiser := &errRaiser{resolveErr: errors.New("resolve boom")}
	j := &ThingOfflineAlertsJob{pool: mock, raiser: raiser, logger: testLogger()}
	if err := j.resolveRecovered(context.Background(), map[string]bool{}); err != nil {
		t.Fatalf("resolveRecovered must not propagate resolve error; got %v", err)
	}
}

// credential_stale_alerts.go resolveRecovered — scan error branch

// TestCredentialStaleAlerts_ResolveRecovered_ScanError covers the scan error
// branch (extra column triggers mismatch).
func TestCredentialStaleAlerts_ResolveRecovered_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(credStaleRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey", "extra"}).AddRow("credential:c1", "boom"))

	j := &CredentialStaleAlertsJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	if err := j.resolveRecovered(context.Background(), map[string]bool{}); err == nil {
		t.Fatal("expected scan error")
	}
}

// agent_cert_expiration_alerts.go: Run — warnDays empty path + band=0 path

// TestAgentCertExpiration_Run_WarnDaysEmpty covers the empty warnDays early
// return (line 79-81): parseWarnDays returns non-empty but warnDaysBand=0.
// We do this by returning an agent whose daysLeft > every warnDays entry.
func TestAgentCertExpiration_Run_BandZeroSkipsRaise(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	loader := &fakeRuleLoader{rule: makeAgentCertRule(true, []any{float64(7)})}

	// Agent expires in 30 days — warnDaysBand([7], 30) = 0 → skip.
	inThirty := time.Now().UTC().Add(30 * 24 * time.Hour)
	mock.ExpectQuery(`FROM thing_agent ta`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "hostname", "cert_expires_at"}).
			AddRow("agent-far", "far-host", inThirty))
	// resolveRecovered: no firing alerts.
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(agentCertExpiryRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	raiser := &fakeRaiser{}
	j := &AgentCertExpirationAlertsJob{pool: mock, raiser: raiser, ruleLoader: loader, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if mock.ExpectationsWereMet() != nil {
		t.Errorf("mock: %v", mock.ExpectationsWereMet())
	}
	// daysLeft=30 > warnDays=[7] → band=0 → no raise.
	if raiser.raiseCount() != 0 {
		t.Errorf("raises = %d, want 0 (band=0)", raiser.raiseCount())
	}
}

// TestAgentCertExpiration_Run_ScanError covers the Scan error branch in Run
// (agent row scan fails).
func TestAgentCertExpiration_Run_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	loader := &fakeRuleLoader{rule: makeAgentCertRule(true, []any{float64(30)})}
	// Return two columns for a three-column scan → scan error.
	mock.ExpectQuery(`FROM thing_agent ta`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "extra"}).AddRow("agent-1", "boom"))

	j := &AgentCertExpirationAlertsJob{pool: mock, raiser: &fakeRaiser{}, ruleLoader: loader, logger: testLogger()}
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected scan error")
	}
}

// TestAgentCertExpiration_ResolveRecovered_ScanError covers the scan error in
// the resolveRecovered helper.
func TestAgentCertExpiration_ResolveRecovered_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(agentCertExpiryRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey", "extra"}).AddRow("thing:agent-1", "boom"))

	j := &AgentCertExpirationAlertsJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	if err := j.resolveRecovered(context.Background(), map[string]bool{}); err == nil {
		t.Fatal("expected scan error")
	}
}

// parseThingOfflineParams — remaining branches

// TestParseThingOfflineParams_WrongType covers the wrong type for
// offlineAfterSec (not float64).
func TestParseThingOfflineParams_WrongType(t *testing.T) {
	_, _, err := parseThingOfflineParams(map[string]any{"offlineAfterSec": "notanumber"})
	if err == nil {
		t.Error("expected error for non-float64 offlineAfterSec")
	}
}

// TestParseThingOfflineParams_ExcludeKindsNotArray covers the wrong type for
// excludeKinds (not []any).
func TestParseThingOfflineParams_ExcludeKindsNotArray(t *testing.T) {
	_, _, err := parseThingOfflineParams(map[string]any{
		"offlineAfterSec": float64(300),
		"excludeKinds":    "not-an-array",
	})
	if err == nil {
		t.Error("expected error for non-array excludeKinds")
	}
}

// TestParseThingOfflineParams_ExcludeKindsNonStringElement covers the non-string
// element inside excludeKinds.
func TestParseThingOfflineParams_ExcludeKindsNonStringElement(t *testing.T) {
	_, _, err := parseThingOfflineParams(map[string]any{
		"offlineAfterSec": float64(300),
		"excludeKinds":    []any{42},
	})
	if err == nil {
		t.Error("expected error for non-string excludeKinds element")
	}
}
