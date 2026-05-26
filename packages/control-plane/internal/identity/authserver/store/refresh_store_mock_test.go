package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

func newRefreshMock(t *testing.T) (pgxmock.PgxPoolIface, *store.RefreshStore) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return mock, store.NewRefreshStoreWithPool(mock)
}

var refreshRowCols = []string{
	"jti", "sessionId", "parentJti", "userId", "clientId", "deviceId",
	"tokenHash", "usedAt", "expiresAt", "createdAt",
}

// TestRefreshStore_Insert_HappyPath asserts the bound args match the
// INSERT column order with parent set to the supplied ParentJTI
// string (non-nil because ParentJTI != "").
func TestRefreshStore_Insert_HappyPath(t *testing.T) {
	mock, s := newRefreshMock(t)
	ctx := context.Background()

	hash := []byte{1, 2, 3, 4}
	dev := "dev_1"
	exp := time.Unix(1_700_001_000, 0).UTC()

	mock.ExpectExec(`INSERT INTO "RefreshToken"`).
		WithArgs("jti_child", "sess_1", "jti_parent", "u_1", "client_1", &dev, hash, exp).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	err := s.Insert(ctx, &store.RefreshTokenRow{
		JTI:       "jti_child",
		SessionID: "sess_1",
		ParentJTI: "jti_parent",
		UserID:    "u_1",
		ClientID:  "client_1",
		DeviceID:  &dev,
		TokenHash: hash,
		ExpiresAt: exp,
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestRefreshStore_Insert_ParentNullWhenEmpty asserts that an empty
// ParentJTI is bound as nil (SQL NULL) so the root-of-chain insert
// doesn't violate the FK on parentJti.
func TestRefreshStore_Insert_ParentNullWhenEmpty(t *testing.T) {
	mock, s := newRefreshMock(t)
	ctx := context.Background()

	hash := []byte{5, 6}
	exp := time.Unix(1_700_002_000, 0).UTC()

	mock.ExpectExec(`INSERT INTO "RefreshToken"`).
		WithArgs("jti_root", "sess_2", nil, "u_2", "client_2", (*string)(nil), hash, exp).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	err := s.Insert(ctx, &store.RefreshTokenRow{
		JTI:       "jti_root",
		SessionID: "sess_2",
		UserID:    "u_2",
		ClientID:  "client_2",
		TokenHash: hash,
		ExpiresAt: exp,
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
}

// TestRefreshStore_Insert_DBError asserts a DB-layer error surfaces
// to the caller verbatim — token issuance must not silently succeed
// when the refresh row failed to persist.
func TestRefreshStore_Insert_DBError(t *testing.T) {
	mock, s := newRefreshMock(t)
	ctx := context.Background()
	boom := errors.New("disk full")

	mock.ExpectExec(`INSERT INTO "RefreshToken"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(boom)

	err := s.Insert(ctx, &store.RefreshTokenRow{
		JTI:       "j",
		SessionID: "s",
		UserID:    "u",
		ClientID:  "c",
		TokenHash: []byte{0},
		ExpiresAt: time.Now().UTC(),
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected DB err to surface; got %v", err)
	}
}

// TestRefreshStore_FindByTokenHash_HappyPath asserts every column
// lands on the returned struct with the expected types (UsedAt nil
// for a fresh row, COALESCE'd parentJti decoded back to empty string).
func TestRefreshStore_FindByTokenHash_HappyPath(t *testing.T) {
	mock, s := newRefreshMock(t)
	ctx := context.Background()

	hash := []byte{9, 8, 7}
	exp := time.Unix(1_700_003_000, 0).UTC()
	created := time.Unix(1_699_900_000, 0).UTC()
	dev := "dev_xyz"

	mock.ExpectQuery(`SELECT jti, "sessionId", COALESCE`).
		WithArgs(hash).
		WillReturnRows(pgxmock.NewRows(refreshRowCols).AddRow(
			"jti_h", "sess_h", "", "u_h", "client_h", &dev, hash,
			(*time.Time)(nil), exp, created,
		))

	got, found, err := s.FindByTokenHash(ctx, hash)
	if err != nil || !found {
		t.Fatalf("FindByTokenHash: found=%v err=%v", found, err)
	}
	if got.JTI != "jti_h" || got.SessionID != "sess_h" || got.ParentJTI != "" {
		t.Fatalf("scan mismatch: %+v", got)
	}
	if !bytes.Equal(got.TokenHash, hash) {
		t.Fatalf("hash not round-tripped: %v vs %v", got.TokenHash, hash)
	}
	if got.UsedAt != nil {
		t.Fatalf("fresh row UsedAt must be nil; got %v", got.UsedAt)
	}
	if !got.ExpiresAt.Equal(exp) || !got.CreatedAt.Equal(created) {
		t.Fatalf("timestamps not round-tripped: %+v", got)
	}
	if got.DeviceID == nil || *got.DeviceID != dev {
		t.Fatalf("deviceId not round-tripped: %v", got.DeviceID)
	}
}

// TestRefreshStore_FindByTokenHash_NotFound asserts (nil,false,nil)
// — not an error — so callers cannot mistakenly treat absence as a
// DB failure.
func TestRefreshStore_FindByTokenHash_NotFound(t *testing.T) {
	mock, s := newRefreshMock(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT jti, "sessionId", COALESCE`).
		WithArgs([]byte{0}).
		WillReturnError(pgx.ErrNoRows)

	got, found, err := s.FindByTokenHash(ctx, []byte{0})
	if got != nil || found || err != nil {
		t.Fatalf("expected (nil,false,nil); got got=%v found=%v err=%v", got, found, err)
	}
}

// TestRefreshStore_FindByTokenHash_GenericError asserts non-ErrNoRows
// surfaces as (nil,false,err) — caller must NOT confuse a DB outage
// with token-not-found.
func TestRefreshStore_FindByTokenHash_GenericError(t *testing.T) {
	mock, s := newRefreshMock(t)
	ctx := context.Background()
	boom := errors.New("io")

	mock.ExpectQuery(`SELECT jti, "sessionId", COALESCE`).
		WithArgs([]byte{1}).
		WillReturnError(boom)

	got, found, err := s.FindByTokenHash(ctx, []byte{1})
	if got != nil || found {
		t.Fatalf("expected nil/false on err; got got=%v found=%v", got, found)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected generic err passthrough; got %v", err)
	}
}

// TestRefreshStore_MarkUsed_Transition asserts a 1-row UPDATE returns
// ok=true (fresh transition: usedAt was NULL → now stamped).
func TestRefreshStore_MarkUsed_Transition(t *testing.T) {
	mock, s := newRefreshMock(t)
	ctx := context.Background()

	mock.ExpectExec(`UPDATE "RefreshToken" SET "usedAt" = NOW\(\) WHERE jti = \$1 AND "usedAt" IS NULL`).
		WithArgs("jti_mark").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	ok, err := s.MarkUsed(ctx, "jti_mark")
	if err != nil || !ok {
		t.Fatalf("expected (true,nil); got ok=%v err=%v", ok, err)
	}
}

// TestRefreshStore_MarkUsed_NoOp asserts ok=false when the conditional
// matched zero rows — caller treats this as replay-detected.
func TestRefreshStore_MarkUsed_NoOp(t *testing.T) {
	mock, s := newRefreshMock(t)
	ctx := context.Background()

	mock.ExpectExec(`UPDATE "RefreshToken"`).
		WithArgs("jti_replay").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	ok, err := s.MarkUsed(ctx, "jti_replay")
	if err != nil || ok {
		t.Fatalf("expected (false,nil); got ok=%v err=%v", ok, err)
	}
}

// TestRefreshStore_MarkUsed_DBError asserts DB error propagates with
// ok=false so caller doesn't double-rotate on a connection drop.
func TestRefreshStore_MarkUsed_DBError(t *testing.T) {
	mock, s := newRefreshMock(t)
	ctx := context.Background()
	boom := errors.New("conn closed")

	mock.ExpectExec(`UPDATE "RefreshToken"`).
		WithArgs("jti_err").
		WillReturnError(boom)

	ok, err := s.MarkUsed(ctx, "jti_err")
	if ok {
		t.Fatalf("ok should be false on err; got true")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected DB err; got %v", err)
	}
}

// TestRefreshStore_DeleteExpired_Success asserts the DELETE fires and
// a successful tag is returned as nil error.
func TestRefreshStore_DeleteExpired_Success(t *testing.T) {
	mock, s := newRefreshMock(t)
	ctx := context.Background()

	mock.ExpectExec(`DELETE FROM "RefreshToken" WHERE "expiresAt" < NOW\(\)`).
		WillReturnResult(pgxmock.NewResult("DELETE", 7))

	if err := s.DeleteExpired(ctx); err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
}

// TestRefreshStore_DeleteExpired_DBError asserts the cleanup job
// surfaces DB errors so the scheduler logs them.
func TestRefreshStore_DeleteExpired_DBError(t *testing.T) {
	mock, s := newRefreshMock(t)
	ctx := context.Background()
	boom := errors.New("lock timeout")

	mock.ExpectExec(`DELETE FROM "RefreshToken" WHERE "expiresAt" < NOW\(\)`).
		WillReturnError(boom)

	if err := s.DeleteExpired(ctx); !errors.Is(err, boom) {
		t.Fatalf("expected DB err; got %v", err)
	}
}

// TestRefreshStore_DeleteBySessionID_Success asserts the session-scoped
// delete fires with the session arg.
func TestRefreshStore_DeleteBySessionID_Success(t *testing.T) {
	mock, s := newRefreshMock(t)
	ctx := context.Background()

	mock.ExpectExec(`DELETE FROM "RefreshToken" WHERE "sessionId" = \$1`).
		WithArgs("sess_x").
		WillReturnResult(pgxmock.NewResult("DELETE", 3))

	if err := s.DeleteBySessionID(ctx, "sess_x"); err != nil {
		t.Fatalf("DeleteBySessionID: %v", err)
	}
}

// TestRefreshStore_DeleteBySessionID_NoOp asserts the documented
// "no rows affected is not an error" contract — logout on a
// session that no longer has any refresh tokens must succeed.
func TestRefreshStore_DeleteBySessionID_NoOp(t *testing.T) {
	mock, s := newRefreshMock(t)
	ctx := context.Background()

	mock.ExpectExec(`DELETE FROM "RefreshToken" WHERE "sessionId" = \$1`).
		WithArgs("sess_empty").
		WillReturnResult(pgxmock.NewResult("DELETE", 0))

	if err := s.DeleteBySessionID(ctx, "sess_empty"); err != nil {
		t.Fatalf("zero-row delete must succeed; got %v", err)
	}
}

// TestRefreshStore_DeleteBySessionID_DBError asserts DB error surfaces.
func TestRefreshStore_DeleteBySessionID_DBError(t *testing.T) {
	mock, s := newRefreshMock(t)
	ctx := context.Background()
	boom := errors.New("io")

	mock.ExpectExec(`DELETE FROM "RefreshToken" WHERE "sessionId" = \$1`).
		WithArgs("sess_err").
		WillReturnError(boom)

	if err := s.DeleteBySessionID(ctx, "sess_err"); !errors.Is(err, boom) {
		t.Fatalf("expected DB err; got %v", err)
	}
}
