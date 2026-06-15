package catbagent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// agentExemptionsCols mirrors the column order in agentExemptionsSelect:
// (target_host, latest).
var agentExemptionsCols = []string{"target_host", "latest"}

// TestAgentExemptionsLoader_Load_Empty pins the empty-result contract:
// when no grants are active, the loader returns the canonical
// {admin_exemptions:[], denylist:[]} shape (NOT an empty/nil state).
// The agent's ApplyShadowState treats `{}` / null as no-ops; we must
// emit the explicit "no admin grants" payload so the agent clears
// stale admin entries from a previous push.
func TestAgentExemptionsLoader_Load_Empty(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectQuery(`FROM compliance_exemption_grant`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(agentExemptionsCols))

	l := NewAgentExemptionsLoader(mock, nil)
	state, ver, err := l.Load(context.Background(), "agent-x")
	if err != nil {
		t.Fatalf("Load err=%v", err)
	}
	if ver != 0 {
		t.Errorf("empty result set should report version=0, got %d", ver)
	}
	raw, _ := json.Marshal(state)
	want := `{"admin_exemptions":[],"denylist":[]}`
	if string(raw) != want {
		t.Errorf("empty state mismatch:\n got %s\nwant %s", raw, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestAgentExemptionsLoader_Load_SingleGrant pins the happy path:
// a single active grant projects target_host into admin_exemptions
// and the version comes from the row's latest (updated_at /
// activated_at) timestamp.
func TestAgentExemptionsLoader_Load_SingleGrant(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	updated := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM compliance_exemption_grant`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(agentExemptionsCols).AddRow(
			"api.openai.com", &updated,
		))

	l := NewAgentExemptionsLoader(mock, nil)
	state, ver, err := l.Load(context.Background(), "agent-x")
	if err != nil {
		t.Fatalf("Load err=%v", err)
	}
	if ver != updated.Unix() {
		t.Errorf("version = %d, want %d (latest.Unix())", ver, updated.Unix())
	}
	raw, _ := json.Marshal(state)
	want := `{"admin_exemptions":["api.openai.com"],"denylist":[]}`
	if string(raw) != want {
		t.Errorf("state mismatch:\n got %s\nwant %s", raw, want)
	}
}

// TestAgentExemptionsLoader_Load_MultipleGrants_VersionIsMax pins
// version aggregation: when several grants are active, the loader
// reports the greatest latest timestamp as the version (so a single
// updated grant bumps version cleanly without re-issuing every row).
func TestAgentExemptionsLoader_Load_MultipleGrants_VersionIsMax(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	older := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`FROM compliance_exemption_grant`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(agentExemptionsCols).
			AddRow("a.example.com", &older).
			AddRow("b.example.com", &newer))

	l := NewAgentExemptionsLoader(mock, nil)
	state, ver, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("Load err=%v", err)
	}
	if ver != newer.Unix() {
		t.Errorf("version should be MAX(latest); got %d want %d", ver, newer.Unix())
	}
	type envelope struct {
		AdminExemptions []string `json:"admin_exemptions"`
		Denylist        []string `json:"denylist"`
	}
	var env envelope
	raw, _ := json.Marshal(state)
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.AdminExemptions) != 2 {
		t.Fatalf("expected 2 hosts, got %d: %+v", len(env.AdminExemptions), env.AdminExemptions)
	}
	if env.AdminExemptions[0] != "a.example.com" || env.AdminExemptions[1] != "b.example.com" {
		t.Errorf("hosts mismatch: %+v", env.AdminExemptions)
	}
	if len(env.Denylist) != 0 {
		t.Errorf("denylist should be empty until CP exposes one; got %+v", env.Denylist)
	}
}

// TestAgentExemptionsLoader_Load_NullLatestTimestamp covers the
// edge case where MAX(updated_at) returns NULL (theoretically
// impossible since updated_at has DEFAULT now(), but a defensive
// scan target is justified). The pgx scanner needs a *time.Time
// to accept a NULL latest column; loader must not blow up.
func TestAgentExemptionsLoader_Load_NullLatestTimestamp(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM compliance_exemption_grant`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(agentExemptionsCols).AddRow(
			"orphan.example.com", (*time.Time)(nil),
		))

	l := NewAgentExemptionsLoader(mock, nil)
	state, ver, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("Load err=%v", err)
	}
	if ver != 0 {
		t.Errorf("NULL latest must produce version=0, got %d", ver)
	}
	raw, _ := json.Marshal(state)
	want := `{"admin_exemptions":["orphan.example.com"],"denylist":[]}`
	if string(raw) != want {
		t.Errorf("state mismatch:\n got %s\nwant %s", raw, want)
	}
}

// TestAgentExemptionsLoader_Load_QueryError surfaces SQL errors to the
// caller. Hub's Cat B handler intentionally does NOT fall back to the
// template row on loader errors (per CatBLoader contract: a transient
// DB blip must not replay an empty payload to the agent).
func TestAgentExemptionsLoader_Load_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM compliance_exemption_grant`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("connection refused"))

	l := NewAgentExemptionsLoader(mock, nil)
	_, _, err := l.Load(context.Background(), "")
	if err == nil {
		t.Fatal("Load must surface query errors")
	}
	if !contains(err.Error(), "query agent exemptions") {
		t.Errorf("error message should wrap the SQL site; got %q", err.Error())
	}
}

// TestAgentExemptionsLoader_Load_ScanError covers a malformed row
// payload (wrong column count). Scan errors propagate up the same
// way query errors do — no partial state should leak.
func TestAgentExemptionsLoader_Load_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Only one column instead of (target_host, latest). The Scan call
	// expects two destinations and will fail.
	mock.ExpectQuery(`FROM compliance_exemption_grant`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"target_host"}).AddRow("a.example.com"))

	l := NewAgentExemptionsLoader(mock, nil)
	_, _, err := l.Load(context.Background(), "")
	if err == nil {
		t.Fatal("scan mismatch must surface as error")
	}
	if !contains(err.Error(), "scan agent exemptions") {
		t.Errorf("error message should wrap the scan site; got %q", err.Error())
	}
}

// TestAgentExemptionsLoader_Load_RowsErrPropagates exercises the
// rows.Err() branch — when the driver reports an iteration error
// after the rows have all been returned (rare, but possible on
// connection drops), the loader surfaces it instead of silently
// truncating the result.
func TestAgentExemptionsLoader_Load_RowsErrPropagates(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM compliance_exemption_grant`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(agentExemptionsCols).
			AddRow("a.example.com", (*time.Time)(nil)).
			CloseError(errors.New("driver: late iter error")))

	l := NewAgentExemptionsLoader(mock, nil)
	_, _, err := l.Load(context.Background(), "")
	if err == nil {
		t.Fatal("rows.Err must surface as Load error")
	}
}

// contains is a tiny substring helper (avoids pulling in strings for
// a single use).
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		ok := true
		for j := range len(needle) {
			if haystack[i+j] != needle[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
