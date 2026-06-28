package catbagent

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

var userInfoCols = []string{
	"id", "displayName", "email", "status", "source", "organizationId", "updatedAt",
}

var orgNodeCols = []string{
	"id", "name", "code", "parentId", "path", "description", "timezone", "updatedAt",
}

// TestUserContext_Load_EmptyThingIDReturnsEmpty covers the early
// short-circuit when thingID is empty — useful for the Hub's
// pre-enrollment bootstrap where no device id is known yet.
// Must NOT query the DB; returns an empty organizations slice and
// ver=0.
func TestUserContext_Load_EmptyThingIDReturnsEmpty(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// No expectations registered — Load must not touch the pool.

	l := NewAgentUserContextLoader(mock, nil)
	state, ver, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ver != 0 {
		t.Errorf("empty thingID must return version=0; got %d", ver)
	}
	raw, _ := json.Marshal(state)
	if string(raw) != `{"organizations":[]}` {
		t.Errorf("empty thingID: %s", raw)
	}
}

// TestUserContext_Load_ActiveAssignment covers the happy path:
// active DeviceAssignment row → user info + org tree resolve →
// version is timestampVersion(max(user.updatedAt, org.updatedAt)),
// i.e. unix-seconds matching every sibling Cat B loader.
func TestUserContext_Load_ActiveAssignment(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	userUpdated := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	orgUpdated := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) // newer → wins
	mock.ExpectQuery(`FROM "DeviceAssignment".*"releasedAt" IS NULL`).
		WithArgs("device-1").
		WillReturnRows(pgxmock.NewRows(userInfoCols).AddRow(
			"user-1", "Alice", "alice@example.com",
			"active", "local", "org-leaf", userUpdated,
		))
	mock.ExpectQuery(`SELECT path FROM "Organization"`).
		WithArgs("org-leaf").
		WillReturnRows(pgxmock.NewRows([]string{"path"}).AddRow("/org-root/org-mid/org-leaf/"))
	mock.ExpectQuery(`FROM "Organization".*ANY`).
		WithArgs([]string{"org-root", "org-mid", "org-leaf"}).
		WillReturnRows(pgxmock.NewRows(orgNodeCols).
			AddRow("org-root", "Root", "ROOT", "", "/org-root/", "", "UTC", orgUpdated.Add(-time.Hour)).
			AddRow("org-mid", "Mid", "MID", "org-root", "/org-root/org-mid/", "", "UTC", orgUpdated.Add(-30*time.Minute)).
			AddRow("org-leaf", "Leaf", "LEAF", "org-mid", "/org-root/org-mid/org-leaf/", "leaf desc", "America/New_York", orgUpdated),
		)

	l := NewAgentUserContextLoader(mock, nil)
	state, ver, err := l.Load(context.Background(), "device-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ver != timestampVersion(orgUpdated) {
		t.Errorf("version = %d, want %d (max of user + org, unix-seconds)", ver, timestampVersion(orgUpdated))
	}
	if ver <= 0 {
		t.Errorf("real timestamps must yield a positive version; got %d", ver)
	}

	raw, _ := json.Marshal(state)
	var got agentUserContextWire
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.User == nil || got.User.ID != "user-1" || got.User.Email != "alice@example.com" {
		t.Errorf("user: %+v", got.User)
	}
	if got.CurrentOrgID != "org-leaf" {
		t.Errorf("CurrentOrgID: %q", got.CurrentOrgID)
	}
	if len(got.Organizations) != 3 {
		t.Fatalf("orgs len: got %d, want 3", len(got.Organizations))
	}
	if got.Organizations[2].ID != "org-leaf" {
		t.Errorf("leaf order: %+v", got.Organizations)
	}
}

// TestUserContext_Load_FallbackToPastAssignment covers the branch
// where active row is missing — the loader retries the same shape
// against the released-at-DESC fallback so the Dashboard has data
// during the logout-to-next-signin window.
func TestUserContext_Load_FallbackToPastAssignment(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	updated := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM "DeviceAssignment".*"releasedAt" IS NULL`).
		WithArgs("device-1").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`FROM "DeviceAssignment".*COALESCE.*releasedAt`).
		WithArgs("device-1").
		WillReturnRows(pgxmock.NewRows(userInfoCols).AddRow(
			"past-user", "Past User", "", "active", "local", "default", updated,
		))
	mock.ExpectQuery(`SELECT path FROM "Organization"`).
		WithArgs("default").
		WillReturnError(pgx.ErrNoRows) // no org row — should yield empty orgs

	l := NewAgentUserContextLoader(mock, nil)
	state, _, err := l.Load(context.Background(), "device-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	raw, _ := json.Marshal(state)
	var got agentUserContextWire
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.User == nil || got.User.ID != "past-user" {
		t.Errorf("fallback user: %+v", got.User)
	}
	if got.CurrentOrgID != "default" {
		t.Errorf("CurrentOrgID: %q", got.CurrentOrgID)
	}
	if len(got.Organizations) != 0 {
		t.Errorf("missing org row should yield empty orgs; got %+v", got.Organizations)
	}
}

// TestUserContext_Load_NoUserUsesDefaultOrg covers the path where
// BOTH assignment queries fail — the loader still emits the
// "default" org chain so pre-enrollment installs see something.
func TestUserContext_Load_NoUserUsesDefaultOrg(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM "DeviceAssignment".*"releasedAt" IS NULL`).
		WithArgs("device-1").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`FROM "DeviceAssignment".*COALESCE.*releasedAt`).
		WithArgs("device-1").
		WillReturnError(pgx.ErrNoRows)
	// Falls back to orgID="default".
	mock.ExpectQuery(`SELECT path FROM "Organization"`).
		WithArgs("default").
		WillReturnRows(pgxmock.NewRows([]string{"path"}).AddRow("/default/"))
	mock.ExpectQuery(`FROM "Organization".*ANY`).
		WithArgs([]string{"default"}).
		WillReturnRows(pgxmock.NewRows(orgNodeCols).AddRow(
			"default", "Default", "DEFAULT", "", "/default/", "", "UTC", time.Now().UTC(),
		))

	l := NewAgentUserContextLoader(mock, nil)
	state, _, err := l.Load(context.Background(), "device-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	raw, _ := json.Marshal(state)
	var got agentUserContextWire
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.User != nil {
		t.Errorf("no-user path should leave User nil; got %+v", got.User)
	}
	if len(got.Organizations) != 1 || got.Organizations[0].ID != "default" {
		t.Errorf("default org chain: %+v", got.Organizations)
	}
}

// TestUserContext_Load_ZeroTimestampsReturnsZeroVersion is the
// regression guard: when both assignment queries miss and the
// resolved org rows carry zero-valued updatedAt timestamps, updatedAt
// stays time.Time{}. The loader MUST report version 0 (via
// timestampVersion's zero-guard), never the large NEGATIVE epoch-
// predecessor that a raw UnixNano() on a zero time produces. A
// negative version would make the configloader treat real later
// pushes as stale.
func TestUserContext_Load_ZeroTimestampsReturnsZeroVersion(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// No active assignment, no past assignment → no user, falls back to
	// orgID="default". The default org row carries a ZERO updatedAt.
	mock.ExpectQuery(`FROM "DeviceAssignment".*"releasedAt" IS NULL`).
		WithArgs("device-zero").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`FROM "DeviceAssignment".*COALESCE.*releasedAt`).
		WithArgs("device-zero").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`SELECT path FROM "Organization"`).
		WithArgs("default").
		WillReturnRows(pgxmock.NewRows([]string{"path"}).AddRow("/default/"))
	mock.ExpectQuery(`FROM "Organization".*ANY`).
		WithArgs([]string{"default"}).
		WillReturnRows(pgxmock.NewRows(orgNodeCols).AddRow(
			"default", "Default", "DEFAULT", "", "/default/", "", "UTC", time.Time{},
		))

	l := NewAgentUserContextLoader(mock, nil)
	_, ver, err := l.Load(context.Background(), "device-zero")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ver != 0 {
		t.Fatalf("zero timestamps must yield version=0; got %d (negative = the F-0118 bug)", ver)
	}
}

// TestLoadOrgAncestors_OrgQueryErrorWraps covers the org-ancestors
// query error branch — must surface "catb: query org ancestors:"
// so the Hub log distinguishes from the install-rule-packs
// loader's identical-shape query.
func TestLoadOrgAncestors_OrgQueryErrorWraps(t *testing.T) {
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
	want := errors.New("connection refused")
	mock.ExpectQuery(`FROM "Organization".*ANY`).
		WithArgs([]string{"leaf"}).
		WillReturnError(want)

	l := NewAgentUserContextLoader(mock, nil)
	_, _, err := l.Load(context.Background(), "device-1")
	if err == nil {
		t.Fatal("expected org-query error to surface")
	}
	if !errors.Is(err, want) {
		t.Errorf("error must wrap original via %%w; got: %v", err)
	}
}

// TestLoadOrgAncestors_EmptyPathReturnsEmpty covers the
// strings.Trim path empty branch — an Organization row whose path
// is just "/" trims to "" and short-circuits with no further
// query.
func TestLoadOrgAncestors_EmptyPathReturnsEmpty(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM "DeviceAssignment".*"releasedAt" IS NULL`).
		WithArgs("device-1").
		WillReturnRows(pgxmock.NewRows(userInfoCols).AddRow(
			"u1", "U1", "", "active", "local", "anon", time.Now().UTC(),
		))
	mock.ExpectQuery(`SELECT path FROM "Organization"`).
		WithArgs("anon").
		WillReturnRows(pgxmock.NewRows([]string{"path"}).AddRow("/")) // just root slash

	l := NewAgentUserContextLoader(mock, nil)
	state, _, err := l.Load(context.Background(), "device-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	raw, _ := json.Marshal(state)
	var got agentUserContextWire
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Organizations) != 0 {
		t.Errorf("empty-path should yield empty orgs; got %+v", got.Organizations)
	}
}
