package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

var tNow = time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)

func anyArgs(n int) []any {
	a := make([]any, n)
	for i := range a {
		a[i] = pgxmock.AnyArg()
	}
	return a
}

func newMock(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(m.Close)
	return New(m), m
}

func TestFindPendingIdentityEvents(t *testing.T) {
	cols := []string{"id", "external_request_id", "thing_id", "source_ip", "entity_id", "identity", "created_at"}

	// Happy: one row with identity JSON + one row with empty identity (decodeJSONB len-0 branch).
	s, m := newMock(t)
	m.ExpectQuery(`FROM traffic_event\s+WHERE identity->>'status' = 'pending'`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows(cols).
			AddRow("e1", "rid1", "thing-1", "1.2.3.4", "ent1", []byte(`{"status":"pending"}`), tNow).
			AddRow("e2", "rid2", "", "5.6.7.8", "ent2", []byte(``), tNow))
	evs, err := s.FindPendingIdentityEvents(context.Background(), time.Hour, 100)
	if err != nil || len(evs) != 2 || evs[0].ID != "e1" || evs[0].Identity["status"] != "pending" || evs[1].Identity != nil {
		t.Fatalf("FindPendingIdentityEvents: %+v err=%v", evs, err)
	}

	// Query error.
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM traffic_event`).WillReturnError(errors.New("boom"))
	if _, err := s2.FindPendingIdentityEvents(context.Background(), time.Hour, 100); err == nil {
		t.Fatal("query error must surface")
	}

	// Scan error (bad created_at).
	s3, m3 := newMock(t)
	m3.ExpectQuery(`FROM traffic_event`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows(cols).AddRow("e1", "rid1", "thing-1", "1.2.3.4", "ent1", []byte(`{}`), "not-a-time"))
	if _, err := s3.FindPendingIdentityEvents(context.Background(), time.Hour, 100); err == nil {
		t.Fatal("scan error must surface")
	}

	// decodeJSONB error (malformed identity JSON).
	s4, m4 := newMock(t)
	m4.ExpectQuery(`FROM traffic_event`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows(cols).AddRow("e1", "rid1", "thing-1", "1.2.3.4", "ent1", []byte(`{bad`), tNow))
	if _, err := s4.FindPendingIdentityEvents(context.Background(), time.Hour, 100); err == nil {
		t.Fatal("decodeJSONB error must surface")
	}
}

var testNow = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

func TestFindMatchedEventByRequestID(t *testing.T) {
	cols := []string{"entity_id", "entity_name", "identity"}

	// Empty request id → ErrNotFound (no query).
	s, m := newMock(t)
	if _, err := s.FindMatchedEventByRequestID(context.Background(), "", "10.0.0.1", testNow, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty request id should be ErrNotFound: %v", err)
	}

	// Happy.
	m.ExpectQuery(`FROM traffic_event\s+WHERE external_request_id = \$1`).WithArgs("rid1", "10.0.0.1", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(cols).AddRow("ent1", "Alice", []byte(`{"status":"matched"}`)))
	got, err := s.FindMatchedEventByRequestID(context.Background(), "rid1", "10.0.0.1", testNow, time.Minute)
	if err != nil || got.EntityID != "ent1" || got.EntityName != "Alice" || got.Identity["status"] != "matched" {
		t.Fatalf("FindMatchedEventByRequestID: %+v %v", got, err)
	}

	// An empty source IP is as unusable as an empty request id: the narrowing
	// that makes this join safe depends on it, so the query must not run at all.
	if _, err := s.FindMatchedEventByRequestID(context.Background(), "rid1", "", testNow, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty source ip should be ErrNotFound before any query: %v", err)
	}

	// ErrNoRows → ErrNotFound.
	m.ExpectQuery(`FROM traffic_event`).WithArgs("gone", "10.0.0.1", pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(pgx.ErrNoRows)
	if _, err := s.FindMatchedEventByRequestID(context.Background(), "gone", "10.0.0.1", testNow, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no rows should be ErrNotFound: %v", err)
	}

	// Other DB error.
	m.ExpectQuery(`FROM traffic_event`).WithArgs("x", "10.0.0.1", pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(errors.New("boom"))
	if _, err := s.FindMatchedEventByRequestID(context.Background(), "x", "10.0.0.1", testNow, time.Minute); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("db error must surface (not ErrNotFound): %v", err)
	}

	// decodeJSONB error.
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM traffic_event`).WithArgs("tr2", "10.0.0.1", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(cols).AddRow("ent2", "Bob", []byte(`{bad`)))
	if _, err := s2.FindMatchedEventByRequestID(context.Background(), "tr2", "10.0.0.1", testNow, time.Minute); err == nil {
		t.Fatal("decodeJSONB error must surface")
	}
}

func TestFindAgentByIP(t *testing.T) {
	cols := []string{"id", "metadata"}

	s, m := newMock(t)
	// Empty IP → ErrNotFound.
	if _, err := s.FindAgentByIP(context.Background(), ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty IP should be ErrNotFound: %v", err)
	}

	// Happy.
	m.ExpectQuery(`FROM thing\s+WHERE type = 'agent'`).WithArgs("1.2.3.4").
		WillReturnRows(pgxmock.NewRows(cols).AddRow("agent1", []byte(`{"hostname":"mac1"}`)))
	got, err := s.FindAgentByIP(context.Background(), "1.2.3.4")
	if err != nil || got.ID != "agent1" || got.Metadata["hostname"] != "mac1" {
		t.Fatalf("FindAgentByIP: %+v %v", got, err)
	}

	// An empty source IP is as unusable as an empty request id: the narrowing
	// that makes this join safe depends on it, so the query must not run at all.
	if _, err := s.FindMatchedEventByRequestID(context.Background(), "rid1", "", testNow, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty source ip should be ErrNotFound before any query: %v", err)
	}

	// ErrNoRows → ErrNotFound.
	m.ExpectQuery(`FROM thing`).WithArgs("9.9.9.9").WillReturnError(pgx.ErrNoRows)
	if _, err := s.FindAgentByIP(context.Background(), "9.9.9.9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no rows should be ErrNotFound: %v", err)
	}

	// Other error.
	m.ExpectQuery(`FROM thing`).WithArgs("8.8.8.8").WillReturnError(errors.New("boom"))
	if _, err := s.FindAgentByIP(context.Background(), "8.8.8.8"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("db error must surface: %v", err)
	}

	// decodeJSONB error.
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM thing`).WithArgs("1.1.1.1").
		WillReturnRows(pgxmock.NewRows(cols).AddRow("agent2", []byte(`{bad`)))
	if _, err := s2.FindAgentByIP(context.Background(), "1.1.1.1"); err == nil {
		t.Fatal("decodeJSONB error must surface")
	}
}

func TestFindActiveAssignmentByIPAndTime(t *testing.T) {
	cols := []string{"user_id", "device_id", "displayName", "email"}

	s, m := newMock(t)
	// Empty IP → ErrNotFound.
	if _, err := s.FindActiveAssignmentByIPAndTime(context.Background(), "", tNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty IP should be ErrNotFound: %v", err)
	}

	// Exactly one → match.
	m.ExpectQuery(`FROM "DeviceAssignment" da`).WithArgs("1.2.3.4", tNow).
		WillReturnRows(pgxmock.NewRows(cols).AddRow("u1", "d1", "Alice", "a@x.com"))
	got, err := s.FindActiveAssignmentByIPAndTime(context.Background(), "1.2.3.4", tNow)
	if err != nil || got.UserID != "u1" || got.Email != "a@x.com" {
		t.Fatalf("single match: %+v %v", got, err)
	}

	// Zero rows → ErrNotFound.
	m.ExpectQuery(`FROM "DeviceAssignment"`).WithArgs("5.5.5.5", tNow).WillReturnRows(pgxmock.NewRows(cols))
	if _, err := s.FindActiveAssignmentByIPAndTime(context.Background(), "5.5.5.5", tNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("zero rows should be ErrNotFound: %v", err)
	}

	// Two rows (shared NAT egress) → ErrAmbiguous.
	m.ExpectQuery(`FROM "DeviceAssignment"`).WithArgs("6.6.6.6", tNow).
		WillReturnRows(pgxmock.NewRows(cols).AddRow("u1", "d1", "Alice", "a@x.com").AddRow("u2", "d2", "Bob", "b@x.com"))
	if _, err := s.FindActiveAssignmentByIPAndTime(context.Background(), "6.6.6.6", tNow); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("two rows should be ErrAmbiguous: %v", err)
	}

	// Query error.
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM "DeviceAssignment"`).WillReturnError(errors.New("boom"))
	if _, err := s2.FindActiveAssignmentByIPAndTime(context.Background(), "1.2.3.4", tNow); err == nil {
		t.Fatal("query error must surface")
	}

	// Scan error (row yields fewer columns than the 4 Scan destinations).
	s3, m3 := newMock(t)
	m3.ExpectQuery(`FROM "DeviceAssignment"`).WithArgs("1.2.3.4", tNow).
		WillReturnRows(pgxmock.NewRows([]string{"user_id", "device_id", "displayName"}).AddRow("u1", "d1", "Alice"))
	if _, err := s3.FindActiveAssignmentByIPAndTime(context.Background(), "1.2.3.4", tNow); err == nil {
		t.Fatal("scan error must surface")
	}

	// Mid-stream iteration error (connection drop after a row yielded).
	s4, m4 := newMock(t)
	m4.ExpectQuery(`FROM "DeviceAssignment"`).WithArgs("1.2.3.4", tNow).
		WillReturnRows(pgxmock.NewRows(cols).AddRow("u1", "d1", "Alice", "a@x.com").CloseError(errors.New("conn reset")))
	if _, err := s4.FindActiveAssignmentByIPAndTime(context.Background(), "1.2.3.4", tNow); err == nil {
		t.Fatal("iterate error must surface")
	}
}

func TestUpdateEventIdentity(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`UPDATE traffic_event\s+SET entity_id`).
		WithArgs("e1", "ent1", "Alice", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	err := s.UpdateEventIdentity(context.Background(), UpdateEventIdentityParams{
		EventID: "e1", EntityID: "ent1", EntityName: "Alice", Identity: map[string]any{"status": "matched"},
	})
	if err != nil {
		t.Fatalf("UpdateEventIdentity: %v", err)
	}

	// Exec error.
	m.ExpectExec(`UPDATE traffic_event`).WithArgs("e1", "ent1", "Alice", pgxmock.AnyArg()).WillReturnError(errors.New("boom"))
	if err := s.UpdateEventIdentity(context.Background(), UpdateEventIdentityParams{
		EventID: "e1", EntityID: "ent1", EntityName: "Alice", Identity: map[string]any{"status": "matched"},
	}); err == nil {
		t.Fatal("exec error must surface")
	}

	// Marshal error: a channel value is not JSON-serialisable → marshal identity fails
	// before any DB call.
	s2, _ := newMock(t)
	if err := s2.UpdateEventIdentity(context.Background(), UpdateEventIdentityParams{
		EventID: "e1", Identity: map[string]any{"bad": make(chan int)},
	}); err == nil {
		t.Fatal("marshal error must surface")
	}
}

// TestFindMatchedEventByRequestID_QueryCarriesEveryNarrowing is a security
// test. The request id on the row being enriched is SELF-REPORTED by the node
// that uploaded it, and the commonest ids are a framework's auto-incrementing
// counter — "1", "2". Without every narrowing below, an enrolled agent could
// attribute its traffic to an arbitrary victim by guessing one, which is the
// forgery the ingest path blanks the self-asserted attribution fields to
// prevent.
//
// The SQL text is pinned deliberately: pgxmock replays columns by position and
// would happily satisfy a query that had quietly lost a WHERE clause.
func TestFindMatchedEventByRequestID_QueryCarriesEveryNarrowing(t *testing.T) {
	for _, clause := range []struct{ name, re string }{
		{"donor must be the gateway", `source = 'ai-gateway'`},
		{"same machine", `source_ip = \$2`},
		{"bounded in time", `created_at BETWEEN \$3 AND \$4`},
		{"donor identity already resolved", `identity->>'status' = 'matched'`},
	} {
		t.Run(clause.name, func(t *testing.T) {
			s, m := newMock(t)
			m.ExpectQuery(clause.re).
				WithArgs("rid1", "10.0.0.1", pgxmock.AnyArg(), pgxmock.AnyArg()).
				WillReturnRows(pgxmock.NewRows([]string{"entity_id", "entity_name", "identity"}).
					AddRow("ent1", "Alice", []byte(`{"status":"matched"}`)))
			if _, err := s.FindMatchedEventByRequestID(
				context.Background(), "rid1", "10.0.0.1", testNow, time.Minute,
			); err != nil {
				t.Fatalf("query lost the %q narrowing — a self-reported request id would match across users: %v", clause.name, err)
			}
		})
	}
}

// TestFindAssignmentByThingAndTime covers the deterministic identity leg: the
// device the Hub authenticated, resolved at the row's own timestamp rather
// than "now", because an audit row is a historical fact and a device may have
// been rebound since.
func TestFindAssignmentByThingAndTime(t *testing.T) {
	cols := []string{"user_id", "device_id", "displayName", "email"}

	// An empty thing id short-circuits before the query. Gateway and
	// compliance-proxy rows carry no device, and querying with "" would match
	// whichever assignment the planner returned first.
	s, _ := newMock(t)
	if _, err := s.FindAssignmentByThingAndTime(context.Background(), "", tNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty thing id should be ErrNotFound before any query: %v", err)
	}

	// Happy path, with the window predicates pinned: an assignment is in force
	// at its own assigned_at and not at its released_at, and the query must ask
	// about the row's timestamp, not the current time.
	s1, m1 := newMock(t)
	m1.ExpectQuery(`WHERE da\."deviceId" = \$1\s+AND da\."assignedAt" <= \$2\s+AND \(da\."releasedAt" IS NULL OR da\."releasedAt" > \$2\)`).
		WithArgs("thing-7", tNow).
		WillReturnRows(pgxmock.NewRows(cols).AddRow("user-7", "thing-7", "Dana", "dana@example.com"))
	got, err := s1.FindAssignmentByThingAndTime(context.Background(), "thing-7", tNow)
	if err != nil || got.UserID != "user-7" || got.DeviceID != "thing-7" || got.Email != "dana@example.com" {
		t.Fatalf("FindAssignmentByThingAndTime: %+v %v", got, err)
	}

	// No assignment covered that instant — the device existed but was unbound.
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM "DeviceAssignment"`).WithArgs("thing-8", tNow).WillReturnError(pgx.ErrNoRows)
	if _, err := s2.FindAssignmentByThingAndTime(context.Background(), "thing-8", tNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no rows should be ErrNotFound: %v", err)
	}

	// A real DB failure must surface as itself, not as "no match" — treating an
	// outage as an absent assignment would stamp a terminal unmatched verdict
	// on rows the job never actually looked at.
	s3, m3 := newMock(t)
	m3.ExpectQuery(`FROM "DeviceAssignment"`).WithArgs("thing-9", tNow).WillReturnError(errors.New("boom"))
	if _, err := s3.FindAssignmentByThingAndTime(context.Background(), "thing-9", tNow); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("db error must surface, not become ErrNotFound: %v", err)
	}
}
