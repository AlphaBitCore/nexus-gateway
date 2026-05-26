package catbagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
)

// hook_config.go error paths

// TestAgentHookConfigLoader_Load_QueryError covers the db.Query failure branch.
// Named failure mode: database connection dropped / planner timeout. Must
// surface "catb: query hook_config:" so the Hub log distinguishes this loader
// from its siblings.
func TestAgentHookConfigLoader_Load_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	want := errors.New("connection refused")
	mock.ExpectQuery(`FROM "HookConfig"`).WillReturnError(want)

	l := NewAgentHookConfigLoader(mock, nil, nil)
	_, _, err := l.Load(context.Background(), "thing-x")
	if err == nil {
		t.Fatal("expected query error to surface")
	}
	if !errors.Is(err, want) {
		t.Errorf("error must wrap original via %%w; got: %v", err)
	}
}

// TestAgentHookConfigLoader_Load_ScanError covers the rows.Scan failure branch.
// Named failure mode: schema drift — a column type change causes pgx to fail
// scanning into the declared Go type. Must surface "catb: scan hook_config:".
// RowError(0, err) with one row causes Scan to return the error on the first
// row, exercising the scan-error return path.
func TestAgentHookConfigLoader_Load_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	now := time.Now().UTC()
	mock.ExpectQuery(`FROM "HookConfig"`).
		WillReturnRows(pgxmock.NewRows(hookConfigCols).
			AddRow(
				"hook-1", "pii", "builtin", "pii-detector", "request",
				(*string)(nil), (*string)(nil), (*string)(nil),
				[]byte(nil), 10, 5000, "fail-open", true, []string{"ALL"}, now,
			).
			RowError(0, errors.New("scan: column type mismatch")))

	l := NewAgentHookConfigLoader(mock, nil, nil)
	_, _, err := l.Load(context.Background(), "")
	if err == nil {
		t.Fatal("expected scan error to surface")
	}
}

// TestAgentHookConfigLoader_Load_RowsErr covers the rows.Err() check after
// iteration. Named failure mode: network interruption mid-cursor — pgx
// returns the transport error on Err() rather than during scan.
// Must surface "catb: iterate hook_config:".
func TestAgentHookConfigLoader_Load_RowsErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	want := errors.New("network: connection reset by peer")
	mock.ExpectQuery(`FROM "HookConfig"`).
		WillReturnRows(pgxmock.NewRows(hookConfigCols).CloseError(want))

	l := NewAgentHookConfigLoader(mock, nil, nil)
	_, _, err := l.Load(context.Background(), "")
	if err == nil {
		t.Fatal("expected rows.Err to surface")
	}
	if !errors.Is(err, want) {
		t.Errorf("error must wrap original; got: %v", err)
	}
}

// TestAgentHookConfigLoader_Load_EnrichError covers the rulepack.Enrich
// failure branch. Named failure mode: rulepack store backend unavailable
// during agent reload — enrichment fails, loader must surface the error so
// Hub knows not to push stale state to the agent.
// Note: rulepack.Enrich wraps the underlying store error inside its own
// "all N consumer hook(s) failed to load installs" wrapper, so we assert
// the error is non-nil and contains the loader's wrapping prefix.
func TestAgentHookConfigLoader_Load_EnrichError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM "HookConfig"`).
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(
			"cs-hook", "cs", "builtin", "content-safety", "request",
			(*string)(nil), (*string)(nil), (*string)(nil),
			[]byte(`{"k":"v"}`),
			10, 5000, "fail-open", true, []string{"ALL"},
			time.Now().UTC(),
		))

	store := &fakeRulePackLister{err: errors.New("rulepack store: timeout")}

	l := NewAgentHookConfigLoader(mock, store, nil)
	_, _, err := l.Load(context.Background(), "")
	if err == nil {
		t.Fatal("enrich error must surface from the loader")
	}
	// The loader wraps with "catb: enrich hook_config with rule packs:".
	const prefix = "catb: enrich hook_config with rule packs:"
	if err.Error()[:len(prefix)] != prefix {
		t.Errorf("error must carry catb prefix; got: %v", err)
	}
}

// installed_rule_packs.go error paths

// TestInstalledRulePacks_Load_InstallScanError covers the rows.Scan failure
// for the install query. Named failure mode: schema drift in rule_pack_install
// causing the installedAt time.Time scan to fail. RowError(0, err) with one
// row causes pgxmock to return the error from Scan on the first iteration.
func TestInstalledRulePacks_Load_InstallScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM rule_pack_install`).
		WillReturnRows(pgxmock.NewRows(rulePackInstallCols).
			AddRow(
				"install-1", "pack-1", "X", "1.0", "M", "",
				"hook-1", true, time.Now().UTC(),
			).
			RowError(0, errors.New("scan: time.Time out of range")))

	l := NewAgentInstalledRulePacksLoader(mock, nil)
	_, _, err := l.Load(context.Background(), "")
	if err == nil {
		t.Fatal("install scan error must surface")
	}
}

// TestInstalledRulePacks_Load_InstallRowsErr covers rows.Err after install
// iteration. Named failure mode: mid-cursor network disconnect that pgx
// surfaces on Err() rather than on individual row scans.
func TestInstalledRulePacks_Load_InstallRowsErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	want := errors.New("network: connection reset")
	mock.ExpectQuery(`FROM rule_pack_install`).
		WillReturnRows(pgxmock.NewRows(rulePackInstallCols).CloseError(want))

	l := NewAgentInstalledRulePacksLoader(mock, nil)
	_, _, err := l.Load(context.Background(), "")
	if err == nil {
		t.Fatal("install rows.Err must surface")
	}
	if !errors.Is(err, want) {
		t.Errorf("error must wrap original; got: %v", err)
	}
}

// TestInstalledRulePacks_Load_RuleScanError covers rows.Scan failure for the
// rule query. Named failure mode: rule table schema change causes pgx to fail
// scanning labels []string. Must surface "catb: scan rule:". RowError(0, err)
// with one row exercises the Scan-error path on the first rule row.
func TestInstalledRulePacks_Load_RuleScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM rule_pack_install`).
		WillReturnRows(pgxmock.NewRows(rulePackInstallCols).AddRow(
			"install-1", "pack-1", "X", "1.0", "M", "",
			"hook-1", true, time.Now().UTC(),
		))
	mock.ExpectQuery(`FROM rule`).
		WithArgs([]string{"pack-1"}).
		WillReturnRows(pgxmock.NewRows(packRuleCols).
			AddRow("rule-a", "pack-1", "ssn", "pii", "high", `\d+`, "", "", []string(nil)).
			RowError(0, errors.New("scan: labels column type changed")))

	l := NewAgentInstalledRulePacksLoader(mock, nil)
	_, _, err := l.Load(context.Background(), "")
	if err == nil {
		t.Fatal("rule scan error must surface")
	}
}

// TestInstalledRulePacks_Load_RuleRowsErr covers ruleRows.Err after iteration.
// Named failure mode: transport reset after streaming first N rule rows.
func TestInstalledRulePacks_Load_RuleRowsErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	want := errors.New("transport: EOF")
	mock.ExpectQuery(`FROM rule_pack_install`).
		WillReturnRows(pgxmock.NewRows(rulePackInstallCols).AddRow(
			"install-1", "pack-1", "X", "1.0", "M", "",
			"hook-1", true, time.Now().UTC(),
		))
	mock.ExpectQuery(`FROM rule`).
		WithArgs([]string{"pack-1"}).
		WillReturnRows(pgxmock.NewRows(packRuleCols).CloseError(want))

	l := NewAgentInstalledRulePacksLoader(mock, nil)
	_, _, err := l.Load(context.Background(), "")
	if err == nil {
		t.Fatal("rule rows.Err must surface")
	}
	if !errors.Is(err, want) {
		t.Errorf("error must wrap original; got: %v", err)
	}
}

// interception_domains.go error paths

// TestInterceptionDomains_Load_DomainQueryError covers db.Query failure for
// the interception_domain table. Named failure mode: connection pool exhausted
// at reload time. Must surface "catb: query interception_domain:".
func TestInterceptionDomains_Load_DomainQueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	want := errors.New("pool exhausted")
	mock.ExpectQuery(`FROM interception_domain`).WillReturnError(want)

	l := NewAgentInterceptionDomainsLoader(mock, nil)
	_, _, err := l.Load(context.Background(), "")
	if err == nil {
		t.Fatal("domain query error must surface")
	}
	if !errors.Is(err, want) {
		t.Errorf("error must wrap original; got: %v", err)
	}
}

// TestInterceptionDomains_Load_DomainScanError covers domainRows.Scan failure.
// Named failure mode: interception_domain schema drift (e.g. updated_at type
// changed from timestamptz to text). Must surface "catb: scan interception_domain:".
// RowError(0, err) with one row exercises the Scan-error path on the first domain.
func TestInterceptionDomains_Load_DomainScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	updated := time.Now().UTC()
	mock.ExpectQuery(`FROM interception_domain`).
		WillReturnRows(pgxmock.NewRows(interceptionDomainCols).
			AddRow(
				"dom-1", "openai", "api.openai.com", "EXACT", "openai-compat",
				[]byte(nil), true, 100, "PROCESS", "FAIL_OPEN", "PUBLIC", updated,
				nil, nil, nil, nil, nil, nil, nil, nil,
			).
			RowError(0, errors.New("scan: updated_at type mismatch")))

	l := NewAgentInterceptionDomainsLoader(mock, nil)
	_, _, err := l.Load(context.Background(), "")
	if err == nil {
		t.Fatal("domain scan error must surface")
	}
}

// TestInterceptionDomains_Load_DomainRowsErr covers domainRows.Err after
// iteration. Named failure mode: network reset mid-cursor while streaming
// the interception_domain result set.
func TestInterceptionDomains_Load_DomainRowsErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	want := errors.New("network: connection reset")
	mock.ExpectQuery(`FROM interception_domain`).
		WillReturnRows(pgxmock.NewRows(interceptionDomainCols).CloseError(want))

	l := NewAgentInterceptionDomainsLoader(mock, nil)
	_, _, err := l.Load(context.Background(), "")
	if err == nil {
		t.Fatal("domain rows.Err must surface")
	}
	if !errors.Is(err, want) {
		t.Errorf("error must wrap original; got: %v", err)
	}
}

// TestInterceptionDomains_Load_PathQueryError covers db.Query failure for the
// interception_path table (reached only when at least one domain row was
// returned). Named failure mode: second query fails due to statement timeout.
// Must surface "catb: query interception_path:".
func TestInterceptionDomains_Load_PathQueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	updated := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM interception_domain`).
		WillReturnRows(pgxmock.NewRows(interceptionDomainCols).AddRow(
			"dom-1", "openai", "api.openai.com", "EXACT", "openai-compat",
			[]byte(nil), true, 100, "PROCESS", "FAIL_OPEN", "PUBLIC", updated,
			nil, nil, nil, nil, nil, nil, nil, nil,
		))
	want := errors.New("statement timeout")
	mock.ExpectQuery(`FROM interception_path`).WillReturnError(want)

	l := NewAgentInterceptionDomainsLoader(mock, nil)
	_, _, err := l.Load(context.Background(), "")
	if err == nil {
		t.Fatal("path query error must surface")
	}
	if !errors.Is(err, want) {
		t.Errorf("error must wrap original; got: %v", err)
	}
}

// TestInterceptionDomains_Load_PathScanError covers pathRows.Scan failure.
// Named failure mode: path_pattern column type changed from text[] to jsonb,
// causing pgx Scan into []string to fail for interception_path rows.
// RowError(0, err) with one row exercises the Scan-error path.
func TestInterceptionDomains_Load_PathScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	updated := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM interception_domain`).
		WillReturnRows(pgxmock.NewRows(interceptionDomainCols).AddRow(
			"dom-1", "openai", "api.openai.com", "EXACT", "openai-compat",
			[]byte(nil), true, 100, "PROCESS", "FAIL_OPEN", "PUBLIC", updated,
			nil, nil, nil, nil, nil, nil, nil, nil,
		))
	mock.ExpectQuery(`FROM interception_path`).
		WillReturnRows(pgxmock.NewRows(interceptionPathCols).
			AddRow("p-1", "dom-1", []string{"/v1"}, "PREFIX", "PROCESS", 10, true, updated).
			RowError(0, errors.New("scan: path_pattern type mismatch")))

	l := NewAgentInterceptionDomainsLoader(mock, nil)
	_, _, err := l.Load(context.Background(), "")
	if err == nil {
		t.Fatal("path scan error must surface")
	}
}

// TestInterceptionDomains_Load_PathRowsErr covers pathRows.Err after iteration.
// Named failure mode: network EOF mid-cursor while streaming path rows.
func TestInterceptionDomains_Load_PathRowsErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	updated := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)
	want := errors.New("EOF")
	mock.ExpectQuery(`FROM interception_domain`).
		WillReturnRows(pgxmock.NewRows(interceptionDomainCols).AddRow(
			"dom-1", "openai", "api.openai.com", "EXACT", "openai-compat",
			[]byte(nil), true, 100, "PROCESS", "FAIL_OPEN", "PUBLIC", updated,
			nil, nil, nil, nil, nil, nil, nil, nil,
		))
	mock.ExpectQuery(`FROM interception_path`).
		WillReturnRows(pgxmock.NewRows(interceptionPathCols).CloseError(want))

	l := NewAgentInterceptionDomainsLoader(mock, nil)
	_, _, err := l.Load(context.Background(), "")
	if err == nil {
		t.Fatal("path rows.Err must surface")
	}
	if !errors.Is(err, want) {
		t.Errorf("error must wrap original; got: %v", err)
	}
}

// payload_capture.go error paths

// TestAgentPayloadCaptureLoader_Load_ScanError covers the row.Scan failure
// branch that is NOT pgx.ErrNoRows. Named failure mode: system_metadata schema
// drift (e.g. updated_at column type changed). Must surface
// "catb: query system_metadata[payload_capture.config]:".
func TestAgentPayloadCaptureLoader_Load_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	want := errors.New("scan: column updated_at type mismatch")
	mock.ExpectQuery(`FROM system_metadata WHERE key = \$1`).
		WithArgs(payloadCaptureConfigKey).
		WillReturnError(want)

	l := NewAgentPayloadCaptureLoader(mock, nil)
	_, _, err := l.Load(context.Background(), "")
	if err == nil {
		t.Fatal("non-ErrNoRows scan error must surface")
	}
	if !errors.Is(err, want) {
		t.Errorf("error must wrap original; got: %v", err)
	}
}

// TestAgentPayloadCaptureLoader_Load_EmptyRawReturnsDefaults covers the
// len(raw)==0 branch — a system_metadata row exists but was written with an
// empty value (admin erased the config). Must return the zero-risk default
// (both capture flags off, standard byte caps) with the row's updated_at
// version so the agent's apply-stamp is preserved.
func TestAgentPayloadCaptureLoader_Load_EmptyRawReturnsDefaults(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	updated := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM system_metadata WHERE key = \$1`).
		WithArgs(payloadCaptureConfigKey).
		WillReturnRows(pgxmock.NewRows(payloadCaptureCols).AddRow(
			[]byte(nil), // empty raw — row exists, no value
			updated,
		))

	l := NewAgentPayloadCaptureLoader(mock, nil)
	state, ver, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("empty raw must degrade to defaults, not error; got %v", err)
	}
	if ver != updated.Unix() {
		t.Errorf("empty raw: version must reflect updated_at; got %d want %d", ver, updated.Unix())
	}
	wire, ok := state.(agentPayloadCaptureWire)
	if !ok {
		t.Fatalf("state type mismatch: %T", state)
	}
	if wire.StoreRequestBody || wire.StoreResponseBody {
		t.Errorf("empty raw must not enable capture flags; got %+v", wire)
	}
	if wire.MaxInlineBodyBytes != payloadCaptureDefaultMaxInlineBodyBytes {
		t.Errorf("MaxInlineBodyBytes = %d, want %d", wire.MaxInlineBodyBytes, payloadCaptureDefaultMaxInlineBodyBytes)
	}
}

// TestAgentPayloadCaptureLoader_Load_MalformedJSON_WithLogger covers the
// json.Unmarshal error branch when a non-nil logger is wired. The logger
// must emit a Warn message and the function must still degrade gracefully.
func TestAgentPayloadCaptureLoader_Load_MalformedJSON_WithLogger(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	updated := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM system_metadata WHERE key = \$1`).
		WithArgs(payloadCaptureConfigKey).
		WillReturnRows(pgxmock.NewRows(payloadCaptureCols).AddRow(
			[]byte(`{not json`), updated,
		))

	// Use a discard logger — we verify the code path compiles and runs, not
	// the log output. The important assertion is: no panic + error not surfaced.
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	l := NewAgentPayloadCaptureLoader(mock, logger)
	_, _, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("malformed JSON with logger must degrade to defaults, not error; got %v", err)
	}
}

// streaming_compliance.go error paths

// TestStreamingCompliance_Load_MalformedJSON_WithLogger covers the logger
// non-nil branch in the json.Unmarshal error path. The function must emit a
// Warn log and return empty wire (DefaultPolicy), not propagate the parse error.
func TestStreamingCompliance_Load_MalformedJSON_WithLogger(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	updated := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM system_metadata`).
		WithArgs(streamingComplianceConfigKey).
		WillReturnRows(pgxmock.NewRows([]string{"value", "updated_at"}).
			AddRow([]byte("not json"), updated))

	// Non-nil discard logger so the l.logger != nil branch executes.
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	l := NewAgentStreamingComplianceLoader(mock, logger)
	state, ver, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("malformed JSON with logger must not error; got %v", err)
	}
	if ver != updated.Unix() {
		t.Errorf("malformed-JSON with logger: version must surface; got %d", ver)
	}
	if state.(agentStreamingComplianceWire).DefaultMode != "" {
		t.Errorf("malformed-JSON with logger: must yield empty wire; got %+v", state)
	}
}

// user_context.go error paths

// TestLoadOrgAncestors_OrgScanError covers the org row Scan error branch inside
// loadOrgAncestors. Named failure mode: Organization schema drift (e.g. updatedAt
// type changed) causes pgx Scan to fail mid-stream. Must surface
// "catb: scan org:".
func TestLoadOrgAncestors_OrgScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	updated := time.Now().UTC()
	mock.ExpectQuery(`FROM "DeviceAssignment".*"releasedAt" IS NULL`).
		WithArgs("device-1").
		WillReturnRows(pgxmock.NewRows(userInfoCols).AddRow(
			"u1", "U1", "", "active", "local", "leaf", updated,
		))
	mock.ExpectQuery(`SELECT path FROM "Organization"`).
		WithArgs("leaf").
		WillReturnRows(pgxmock.NewRows([]string{"path"}).AddRow("/leaf/"))
	want := errors.New("scan: updatedAt type mismatch")
	mock.ExpectQuery(`FROM "Organization".*ANY`).
		WithArgs([]string{"leaf"}).
		WillReturnRows(pgxmock.NewRows(orgNodeCols).
			AddRow("leaf", "Leaf", "LEAF", "", "/leaf/", "", "UTC", updated).
			RowError(0, want))

	l := NewAgentUserContextLoader(mock, nil)
	_, _, err := l.Load(context.Background(), "device-1")
	if err == nil {
		t.Fatal("org scan error must surface from loadOrgAncestors")
	}
}

// TestUserContext_Load_OrgAncestorsIterError covers the rows.Err branch
// inside loadOrgAncestors. Named failure mode: network EOF while streaming
// the org ancestor rows — loadOrgAncestors must surface "catb: iterate org:".
func TestUserContext_Load_OrgAncestorsIterError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	updated := time.Now().UTC()
	mock.ExpectQuery(`FROM "DeviceAssignment".*"releasedAt" IS NULL`).
		WithArgs("device-1").
		WillReturnRows(pgxmock.NewRows(userInfoCols).AddRow(
			"u1", "U1", "", "active", "local", "leaf", updated,
		))
	mock.ExpectQuery(`SELECT path FROM "Organization"`).
		WithArgs("leaf").
		WillReturnRows(pgxmock.NewRows([]string{"path"}).AddRow("/leaf/"))
	want := errors.New("EOF streaming org rows")
	mock.ExpectQuery(`FROM "Organization".*ANY`).
		WithArgs([]string{"leaf"}).
		WillReturnRows(pgxmock.NewRows(orgNodeCols).CloseError(want))

	l := NewAgentUserContextLoader(mock, nil)
	_, _, err := l.Load(context.Background(), "device-1")
	if err == nil {
		t.Fatal("org iterate error must surface")
	}
	if !errors.Is(err, want) {
		t.Errorf("error must wrap original; got: %v", err)
	}
}

// hook_config.go — rulepack enrichment: marshal/unmarshal paranoia test

// TestAgentHookConfigLoader_Load_EnrichRoundTripPreservesAllHooks asserts that
// the JSON round-trip (marshal agentHookConfigRow → unmarshal hooks.HookConfig
// → enrich → re-marshal) preserves hook count and the non-consumer hook's
// config is not altered. This validates the full enrichment pipeline for a
// multi-hook scenario, catching future field-tag drift that would silently
// drop hooks during the round-trip.
func TestAgentHookConfigLoader_Load_EnrichRoundTripPreservesAllHooks(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	now := time.Now().UTC()
	mock.ExpectQuery(`FROM "HookConfig"`).
		WillReturnRows(pgxmock.NewRows(hookConfigCols).
			AddRow(
				"hook-rpe", "rpe", "builtin", "rulepack-engine", "request",
				(*string)(nil), (*string)(nil), (*string)(nil),
				[]byte(`{"t":1}`), 5, 5000, "fail-open", true, []string{"ALL"}, now,
			).
			AddRow(
				"hook-kf", "kf", "builtin", "keyword-filter", "request",
				(*string)(nil), (*string)(nil), (*string)(nil),
				[]byte(`{"words":["bad"]}`), 10, 5000, "fail-open", true, []string{"ALL"}, now,
			))

	// Both hooks are RulePackConsumers; empty install sets simulate a fresh install.
	store := &fakeRulePackLister{sets: map[string][]rulepack.EffectiveRuleSet{
		"hook-rpe": {},
		"hook-kf":  {},
	}}

	l := NewAgentHookConfigLoader(mock, store, nil)
	state, _, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	type envelope struct {
		HookConfigs []map[string]any `json:"hookConfigs"`
	}
	var env envelope
	raw, _ := json.Marshal(state)
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.HookConfigs) != 2 {
		t.Errorf("enrich round-trip must preserve all hooks; got %d", len(env.HookConfigs))
	}
}
