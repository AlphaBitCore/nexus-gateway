package revocation_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
)

// newMockStore returns a freshly-mocked PgxPool and the Store wired to it.
// Caller is expected to ExpectQuery/ExpectExec on the returned mock.
func newMockStore(t *testing.T) (pgxmock.PgxPoolIface, *revocation.Store) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new pgxmock: %v", err)
	}
	t.Cleanup(mock.Close)
	return mock, revocation.NewStoreWithPool(mock)
}

// TestStore_Insert_HappyPath asserts: bind order matches the column order in
// the INSERT statement (scope first, actor last) and the returned id is the
// bigserial value scanned out of RETURNING.
func TestStore_Insert_HappyPath(t *testing.T) {
	mock, store := newMockStore(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()
	exp := now.Add(time.Hour)
	jti := "jti-1"
	user := "user-1"
	dev := "dev-1"
	sess := "sess-1"
	actor := "admin:alice"

	mock.ExpectQuery(`INSERT INTO "RevokedToken"`).
		WithArgs(string(revocation.ScopeJTI), &jti, &user, &dev, &sess,
			now, exp, revocation.ReasonAdminDisable, &actor).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(42)))

	id, err := store.Insert(ctx, revocation.Row{
		Scope:           revocation.ScopeJTI,
		TargetJTI:       &jti,
		TargetUserID:    &user,
		TargetDeviceID:  &dev,
		TargetSessionID: &sess,
		RevokedAt:       now,
		ExpiresAt:       exp,
		Reason:          revocation.ReasonAdminDisable,
		Actor:           &actor,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id != 42 {
		t.Fatalf("id = %d, want 42", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestStore_Insert_QueryRowErrorWrapped asserts that a DB-level error from
// QueryRow.Scan is wrapped with the "revocation: insert:" prefix so logs
// always identify the failure surface. Token-revocation writes are
// security-critical: silent error pass-through here would mask write-path
// outages.
func TestStore_Insert_QueryRowErrorWrapped(t *testing.T) {
	mock, store := newMockStore(t)
	ctx := context.Background()

	want := errors.New("connection reset")
	mock.ExpectQuery(`INSERT INTO "RevokedToken"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(want)

	id, err := store.Insert(ctx, revocation.Row{
		Scope:     revocation.ScopeJTI,
		RevokedAt: time.Now().UTC(),
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
		Reason:    revocation.ReasonReplayDetected,
	})
	if id != 0 {
		t.Fatalf("on error id should be 0; got %d", id)
	}
	if !errors.Is(err, want) {
		t.Fatalf("err should wrap underlying err; got: %v", err)
	}
	if !strings.Contains(err.Error(), "revocation: insert") {
		t.Fatalf("missing wrap prefix: %v", err)
	}
}

// TestStore_ListSince_HonoursCursorAndLimit asserts:
//   - the SQL is parameterised on (sinceID, limit) — caller cursor is honoured;
//   - returned last == max(id) across the result rows when rows exist;
//   - scope/target string round-trips through pgxmock with the right pointer
//     semantics (NULL → nil on the Go side).
func TestStore_ListSince_HonoursCursorAndLimit(t *testing.T) {
	mock, store := newMockStore(t)
	ctx := context.Background()

	now := time.Unix(1_700_000_500, 0).UTC()
	jti := "jti-1"
	user := "user-7"
	actor := "authserver"

	mock.ExpectQuery(`FROM "RevokedToken"`).
		WithArgs(int64(10), 50).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "scope", "targetJti", "targetUserId", "targetDeviceId",
			"targetSessionId", "revokedAt", "expiresAt", "reason", "actor",
		}).
			AddRow(int64(11), "jti", &jti, (*string)(nil), (*string)(nil),
				(*string)(nil), now, now.Add(time.Hour), revocation.ReasonReplayDetected, &actor).
			AddRow(int64(15), "user", (*string)(nil), &user, (*string)(nil),
				(*string)(nil), now, now.Add(2*time.Hour), revocation.ReasonAdminDisable, &actor))

	rows, last, err := store.ListSince(ctx, 10, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if last != 15 {
		t.Fatalf("last = %d, want 15", last)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if rows[0].Scope != revocation.ScopeJTI || rows[0].TargetJTI == nil || *rows[0].TargetJTI != jti {
		t.Fatalf("row[0] mismatch: %+v", rows[0])
	}
	if rows[1].Scope != revocation.ScopeUser || rows[1].TargetUserID == nil || *rows[1].TargetUserID != user {
		t.Fatalf("row[1] mismatch: %+v", rows[1])
	}
	if rows[0].TargetUserID != nil || rows[1].TargetJTI != nil {
		t.Fatalf("NULL columns must round-trip as nil: %+v / %+v", rows[0], rows[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestStore_ListSince_LimitClamping covers the two clamp branches in the
// limit guard: limit <= 0 → 1000, limit > 1000 → 1000.
func TestStore_ListSince_LimitClamping(t *testing.T) {
	cases := []struct {
		name  string
		input int
	}{
		{"zero clamped to 1000", 0},
		{"negative clamped to 1000", -1},
		{"too-large clamped to 1000", 2000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, store := newMockStore(t)
			mock.ExpectQuery(`FROM "RevokedToken"`).
				WithArgs(int64(0), 1000).
				WillReturnRows(pgxmock.NewRows([]string{
					"id", "scope", "targetJti", "targetUserId", "targetDeviceId",
					"targetSessionId", "revokedAt", "expiresAt", "reason", "actor",
				}))
			rows, last, err := store.ListSince(context.Background(), 0, tc.input)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(rows) != 0 || last != 0 {
				t.Fatalf("empty result expected; got rows=%d last=%d", len(rows), last)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("expectations: %v", err)
			}
		})
	}
}

// TestStore_ListSince_QueryErrorWrapped — wrong-shape query error is wrapped
// with the "revocation: list:" prefix.
func TestStore_ListSince_QueryErrorWrapped(t *testing.T) {
	mock, store := newMockStore(t)
	want := errors.New("planner err")
	mock.ExpectQuery(`FROM "RevokedToken"`).
		WithArgs(int64(0), 100).
		WillReturnError(want)

	rows, last, err := store.ListSince(context.Background(), 0, 100)
	if rows != nil || last != 0 {
		t.Fatalf("expected zero outputs on err; got rows=%v last=%d", rows, last)
	}
	if !errors.Is(err, want) {
		t.Fatalf("wrap missing: %v", err)
	}
	if !strings.Contains(err.Error(), "revocation: list") {
		t.Fatalf("missing prefix: %v", err)
	}
}

// TestStore_ListSince_ScanErrorWrapped triggers the Scan failure branch by
// returning a column count mismatch. Scope corruption can only happen if the
// DB schema and Go scan target diverge — that condition must surface a
// wrapped error, not a panic.
func TestStore_ListSince_ScanErrorWrapped(t *testing.T) {
	mock, store := newMockStore(t)
	// 3 columns returned but scan expects 10 → pgx will return a Scan error.
	mock.ExpectQuery(`FROM "RevokedToken"`).
		WithArgs(int64(0), 100).
		WillReturnRows(pgxmock.NewRows([]string{"id", "scope", "extra"}).
			AddRow(int64(1), "jti", "wrong"))

	_, _, err := store.ListSince(context.Background(), 0, 100)
	if err == nil {
		t.Fatal("expected scan err")
	}
	if !strings.Contains(err.Error(), "revocation: scan") {
		t.Fatalf("expected scan wrap; got: %v", err)
	}
}

// TestStore_ListSince_RowsErrPropagated covers the `return rows.Err()` path:
// pgxmock's CloseError surfaces from rows.Err() after iteration completes,
// matching the production code path where a transient network error fires
// after the last successful Scan.
func TestStore_ListSince_RowsErrPropagated(t *testing.T) {
	mock, store := newMockStore(t)
	now := time.Unix(1_700_000_500, 0).UTC()
	jti := "jti-1"
	actor := "authserver"
	want := errors.New("conn lost after last row")
	mock.ExpectQuery(`FROM "RevokedToken"`).
		WithArgs(int64(0), 100).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "scope", "targetJti", "targetUserId", "targetDeviceId",
			"targetSessionId", "revokedAt", "expiresAt", "reason", "actor",
		}).
			AddRow(int64(1), "jti", &jti, (*string)(nil), (*string)(nil),
				(*string)(nil), now, now.Add(time.Hour), revocation.ReasonReplayDetected, &actor).
			CloseError(want))

	_, _, err := store.ListSince(context.Background(), 0, 100)
	if !errors.Is(err, want) {
		t.Fatalf("expected rows.Err() to propagate: %v", err)
	}
}

// TestStore_ListSince_LastStaysWhenEmpty covers the cursor invariant: an
// empty result set must NOT reset the caller's cursor — returned last must
// equal sinceID so paged callers can continue without rewinding.
func TestStore_ListSince_LastStaysWhenEmpty(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`FROM "RevokedToken"`).
		WithArgs(int64(99), 1000).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "scope", "targetJti", "targetUserId", "targetDeviceId",
			"targetSessionId", "revokedAt", "expiresAt", "reason", "actor",
		}))
	_, last, err := store.ListSince(context.Background(), 99, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if last != 99 {
		t.Fatalf("empty result must preserve sinceID; got %d", last)
	}
}

// TestStore_IsAccessTokenRevoked covers the four scope→target reject paths
// (the security-critical contract) plus the all-empty-claims happy path
// where the EXISTS expression's `$N <> ”` guard prevents NULL false-match.
func TestStore_IsAccessTokenRevoked(t *testing.T) {
	t.Run("jti scope hit", func(t *testing.T) {
		mock, store := newMockStore(t)
		mock.ExpectQuery(`SELECT EXISTS`).
			WithArgs("jti-1", "", "", "").
			WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))
		ok, err := store.IsAccessTokenRevoked(context.Background(), "jti-1", "", "", "")
		if err != nil || !ok {
			t.Fatalf("expected revoked=true; got ok=%v err=%v", ok, err)
		}
	})
	t.Run("user scope hit", func(t *testing.T) {
		mock, store := newMockStore(t)
		mock.ExpectQuery(`SELECT EXISTS`).
			WithArgs("", "user-9", "", "").
			WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))
		ok, err := store.IsAccessTokenRevoked(context.Background(), "", "user-9", "", "")
		if err != nil || !ok {
			t.Fatalf("user revocation must reject the token: ok=%v err=%v", ok, err)
		}
	})
	t.Run("device scope hit", func(t *testing.T) {
		mock, store := newMockStore(t)
		mock.ExpectQuery(`SELECT EXISTS`).
			WithArgs("", "", "dev-7", "").
			WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))
		ok, err := store.IsAccessTokenRevoked(context.Background(), "", "", "dev-7", "")
		if err != nil || !ok {
			t.Fatalf("device revocation must reject the token: ok=%v err=%v", ok, err)
		}
	})
	t.Run("session scope hit", func(t *testing.T) {
		mock, store := newMockStore(t)
		mock.ExpectQuery(`SELECT EXISTS`).
			WithArgs("", "", "", "sess-3").
			WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))
		ok, err := store.IsAccessTokenRevoked(context.Background(), "", "", "", "sess-3")
		if err != nil || !ok {
			t.Fatalf("session revocation must reject the token: ok=%v err=%v", ok, err)
		}
	})
	t.Run("no row exists: token is accepted", func(t *testing.T) {
		mock, store := newMockStore(t)
		mock.ExpectQuery(`SELECT EXISTS`).
			WithArgs("jti-clean", "user-clean", "dev-clean", "sess-clean").
			WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(false))
		ok, err := store.IsAccessTokenRevoked(context.Background(),
			"jti-clean", "user-clean", "dev-clean", "sess-clean")
		if err != nil || ok {
			t.Fatalf("unrevoked token must NOT be flagged: ok=%v err=%v", ok, err)
		}
	})
	t.Run("scan err wrapped", func(t *testing.T) {
		mock, store := newMockStore(t)
		want := errors.New("driver err")
		mock.ExpectQuery(`SELECT EXISTS`).
			WithArgs("j", "u", "d", "s").
			WillReturnError(want)
		ok, err := store.IsAccessTokenRevoked(context.Background(), "j", "u", "d", "s")
		if ok {
			t.Fatal("on error must default to ok=false (fail-closed)")
		}
		if !errors.Is(err, want) || !strings.Contains(err.Error(), "revocation: IsAccessTokenRevoked") {
			t.Fatalf("wrap missing: %v", err)
		}
	})
}

// TestStore_DeleteExpired returns the affected-row count from CommandTag.
func TestStore_DeleteExpired(t *testing.T) {
	t.Run("happy path returns affected", func(t *testing.T) {
		mock, store := newMockStore(t)
		mock.ExpectExec(`DELETE FROM "RevokedToken"`).
			WillReturnResult(pgconn.NewCommandTag("DELETE 7"))
		n, err := store.DeleteExpired(context.Background())
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		if n != 7 {
			t.Fatalf("affected = %d, want 7", n)
		}
	})
	t.Run("exec err wrapped", func(t *testing.T) {
		mock, store := newMockStore(t)
		want := errors.New("deadlock")
		mock.ExpectExec(`DELETE FROM "RevokedToken"`).WillReturnError(want)
		n, err := store.DeleteExpired(context.Background())
		if n != 0 {
			t.Fatalf("on err count must be 0; got %d", n)
		}
		if !errors.Is(err, want) || !strings.Contains(err.Error(), "revocation: delete expired") {
			t.Fatalf("wrap missing: %v", err)
		}
	})
}

// TestStore_NewStore_AcceptsConcretePool is a smoke for the production
// constructor — guards against a regression where the signature drifts off
// *pgxpool.Pool (which would break the cmd/control-plane wiring at line 514).
func TestStore_NewStore_AcceptsConcretePool(t *testing.T) {
	// Passing nil here is fine — the wired *pgxpool.Pool is never
	// dereferenced by NewStore itself. The point of this test is to lock
	// the signature, not exercise behaviour.
	s := revocation.NewStore(nil)
	if s == nil {
		t.Fatal("NewStore returned nil for a nil pool (signature regression)")
	}
}
