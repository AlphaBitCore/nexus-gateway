package catbagent

import (
	"context"
	"encoding/json"
	"errors"
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
