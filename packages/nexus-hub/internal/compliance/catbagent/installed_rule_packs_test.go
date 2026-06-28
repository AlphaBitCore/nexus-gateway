package catbagent

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

var rulePackInstallCols = []string{
	"id", "packId", "name", "pinVersion", "maintainer", "description",
	"boundHookId", "enabled", "installedAt",
}

var packRuleCols = []string{
	"id", "packId", "ruleId", "category", "severity", "pattern",
	"flags", "description", "labels",
}

var ruleOverrideCols = []string{"installId", "ruleLocalId", "disabled", "severityOverride"}

// TestInstalledRulePacks_Load_Empty covers the no-installs branch:
// no rule_pack_install rows means the rule-loader query is skipped
// (len(packIDs) > 0 guard) and the response is an empty array with
// version=0. Pinned because parseRulePacks on the agent side keys
// on `installedRulePacks` being a (possibly empty) array, not nil.
func TestInstalledRulePacks_Load_Empty(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM rule_pack_install`).
		WillReturnRows(pgxmock.NewRows(rulePackInstallCols))

	l := NewAgentInstalledRulePacksLoader(mock, nil)
	state, ver, err := l.Load(context.Background(), "thing-x")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ver != 0 {
		t.Errorf("empty result must report version=0; got %d", ver)
	}
	raw, _ := json.Marshal(state)
	if string(raw) != `{"installedRulePacks":[]}` {
		t.Errorf("empty: got %s", raw)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestInstalledRulePacks_Load_OnePackWithRules covers the happy
// path: one install row → second query for that pack's rules →
// rules attached to the right pack, RuleCount tallied, version =
// installedAt.UnixNano().
func TestInstalledRulePacks_Load_OnePackWithRules(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	installedAt := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM rule_pack_install`).
		WillReturnRows(pgxmock.NewRows(rulePackInstallCols).AddRow(
			"install-1", "pack-1", "Acme PII", "1.2.0", "Acme", "PII rules",
			"hook-1", true, installedAt,
		))
	mock.ExpectQuery(`FROM rule`).
		WithArgs([]string{"pack-1"}).
		WillReturnRows(pgxmock.NewRows(packRuleCols).
			AddRow("rule-a", "pack-1", "ssn", "pii", "high", `\d{3}-\d{2}-\d{4}`, "", "", []string{"pii"}).
			AddRow("rule-b", "pack-1", "phone", "pii", "med", `\d{3}-\d{4}`, "i", "phone number", []string(nil)),
		)
	mock.ExpectQuery(`FROM rule_override`).
		WithArgs([]string{"install-1"}).
		WillReturnRows(pgxmock.NewRows(ruleOverrideCols))

	l := NewAgentInstalledRulePacksLoader(mock, nil)
	state, ver, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ver != installedAt.Unix() {
		t.Errorf("version: got %d, want %d (unix seconds)", ver, installedAt.Unix())
	}

	type packShape struct {
		PackID      string `json:"packId"`
		RuleCount   int    `json:"ruleCount"`
		InstalledAt string `json:"installedAt"`
		Rules       []struct {
			RuleID   string   `json:"ruleId"`
			Category string   `json:"category"`
			Labels   []string `json:"labels,omitempty"`
		} `json:"rules"`
	}
	var got struct {
		InstalledRulePacks []packShape `json:"installedRulePacks"`
	}
	raw, _ := json.Marshal(state)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if len(got.InstalledRulePacks) != 1 {
		t.Fatalf("packs len: got %d, want 1", len(got.InstalledRulePacks))
	}
	p := got.InstalledRulePacks[0]
	if p.PackID != "pack-1" || p.RuleCount != 2 {
		t.Errorf("pack: %+v", p)
	}
	if len(p.Rules) != 2 {
		t.Errorf("rules len: %d", len(p.Rules))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestInstalledRulePacks_Load_OverrideDisablesAndReseverities pins the override
// application: a disabled rule must NOT appear in the agent's view (it isn't
// scanned), and a severityOverride must replace the rule's base severity — so the
// read-only Policies page shows exactly what the scanner enforces
// (rulepack.LoadForInstall semantics). Without this, an admin who disabled a rule
// would still see it as active on the device.
func TestInstalledRulePacks_Load_OverrideDisablesAndReseverities(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	installedAt := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM rule_pack_install`).
		WillReturnRows(pgxmock.NewRows(rulePackInstallCols).AddRow(
			"install-1", "pack-1", "Acme PII", "1.2.0", "Acme", "PII rules",
			"hook-1", true, installedAt,
		))
	mock.ExpectQuery(`FROM rule`).
		WithArgs([]string{"pack-1"}).
		WillReturnRows(pgxmock.NewRows(packRuleCols).
			AddRow("rule-a", "pack-1", "ssn", "pii", "high", `\d{3}-\d{2}-\d{4}`, "", "", []string{"pii"}).
			AddRow("rule-b", "pack-1", "ipv4", "pii.network", "warn", `\d+\.\d+\.\d+\.\d+`, "", "", []string(nil)).
			AddRow("rule-c", "pack-1", "phone", "pii", "warn", `\d{3}-\d{4}`, "", "", []string(nil)),
		)
	// rule-b disabled; rule-c severity bumped warn -> high.
	mock.ExpectQuery(`FROM rule_override`).
		WithArgs([]string{"install-1"}).
		WillReturnRows(pgxmock.NewRows(ruleOverrideCols).
			AddRow("install-1", "ipv4", true, "").
			AddRow("install-1", "phone", false, "high"),
		)

	l := NewAgentInstalledRulePacksLoader(mock, nil)
	state, _, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var got struct {
		InstalledRulePacks []struct {
			RuleCount int `json:"ruleCount"`
			Rules     []struct {
				RuleID   string `json:"ruleId"`
				Severity string `json:"severity"`
			} `json:"rules"`
		} `json:"installedRulePacks"`
	}
	raw, _ := json.Marshal(state)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if len(got.InstalledRulePacks) != 1 {
		t.Fatalf("packs len: %d", len(got.InstalledRulePacks))
	}
	p := got.InstalledRulePacks[0]
	if p.RuleCount != 2 {
		t.Errorf("disabled rule must be dropped: ruleCount got %d, want 2", p.RuleCount)
	}
	bySev := map[string]string{}
	for _, r := range p.Rules {
		bySev[r.RuleID] = r.Severity
		if r.RuleID == "ipv4" {
			t.Errorf("disabled rule 'ipv4' must not appear")
		}
	}
	if bySev["phone"] != "high" {
		t.Errorf("severityOverride not applied: phone severity got %q, want high", bySev["phone"])
	}
	if bySev["ssn"] != "high" {
		t.Errorf("unoverridden rule severity changed: ssn got %q, want high", bySev["ssn"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestInstalledRulePacks_Load_OverrideQueryError covers the override-query
// error branch — rules loaded fine but the rule_override fetch failed. The
// wrapped err must say "catb: query rule_override:" so the operator knows the
// override lookup is the suspect (distinct from the rule fetch).
func TestInstalledRulePacks_Load_OverrideQueryError(t *testing.T) {
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
			AddRow("rule-a", "pack-1", "ssn", "pii", "high", `x`, "", "", []string(nil)),
		)
	want := errors.New("override timeout")
	mock.ExpectQuery(`FROM rule_override`).WithArgs([]string{"install-1"}).WillReturnError(want)

	l := NewAgentInstalledRulePacksLoader(mock, nil)
	_, _, err := l.Load(context.Background(), "")
	if err == nil {
		t.Fatal("expected override-query error")
	}
	if !errors.Is(err, want) {
		t.Errorf("error must wrap original; got: %v", err)
	}
}

// TestInstalledRulePacks_Load_InstallQueryError covers the first
// query's error wrap — a Postgres planner / connection error must
// surface "catb: query rule_pack_install:" so the Hub log
// distinguishes this loader's failure from sibling Cat B loaders.
func TestInstalledRulePacks_Load_InstallQueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	want := errors.New("connection refused")
	mock.ExpectQuery(`FROM rule_pack_install`).WillReturnError(want)

	l := NewAgentInstalledRulePacksLoader(mock, nil)
	_, _, err := l.Load(context.Background(), "")
	if err == nil {
		t.Fatal("expected query error")
	}
	if !errors.Is(err, want) {
		t.Errorf("error must wrap original via %%w; got: %v", err)
	}
}

// TestInstalledRulePacks_Load_RuleQueryError covers the
// second-query error branch — install rows succeeded but the rule
// fetch failed. The wrapped err must say "catb: query rule:" so
// the operator knows the second query is the suspect.
func TestInstalledRulePacks_Load_RuleQueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM rule_pack_install`).
		WillReturnRows(pgxmock.NewRows(rulePackInstallCols).AddRow(
			"install-1", "pack-1", "X", "1.0", "M", "",
			"hook-1", true, time.Now().UTC(),
		))
	want := errors.New("timeout")
	mock.ExpectQuery(`FROM rule`).WithArgs([]string{"pack-1"}).WillReturnError(want)

	l := NewAgentInstalledRulePacksLoader(mock, nil)
	_, _, err := l.Load(context.Background(), "")
	if err == nil {
		t.Fatal("expected rule-query error")
	}
	if !errors.Is(err, want) {
		t.Errorf("error must wrap original; got: %v", err)
	}
}
