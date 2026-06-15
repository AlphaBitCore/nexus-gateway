package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

// Tests for the simpler 0% thing_registry methods. The large
// UpsertThingEnrollment / UpsertThingEnrollmentWithDesiredVer
// upserts are covered by a separate dedicated test file (their
// CASE-WHEN logic + jsonb merge deserves a focused suite).

// TestUpsertThingService covers happy + default-role + err return.
func TestUpsertThingService(t *testing.T) {
	t.Run("explicit role", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`INSERT INTO thing_service`).
			WithArgs("thing-1", "primary", "http://m", "http://mgmt").
			WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
		store := New(mock)
		if err := store.UpsertThingService(context.Background(), "thing-1", "http://m", "http://mgmt", "primary"); err != nil {
			t.Fatalf("UpsertThingService: %v", err)
		}
	})
	t.Run("empty role defaults to 'default'", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`INSERT INTO thing_service`).
			WithArgs("thing-1", "default", "", "").
			WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
		store := New(mock)
		if err := store.UpsertThingService(context.Background(), "thing-1", "", "", ""); err != nil {
			t.Fatalf("UpsertThingService: %v", err)
		}
	})
	t.Run("exec err propagates", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("constraint violation")
		mock.ExpectExec(`INSERT INTO thing_service`).
			WithArgs("t", "default", "", "").
			WillReturnError(want)
		store := New(mock)
		if err := store.UpsertThingService(context.Background(), "t", "", "", ""); !errors.Is(err, want) {
			t.Errorf("must propagate; got: %v", err)
		}
	})
}

// TestGetThingManagementURL covers happy + null managementURL +
// not-found + generic err wrap.
func TestGetThingManagementURL(t *testing.T) {
	t.Run("happy non-null", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		url := "http://mgmt.example"
		mock.ExpectQuery(`FROM thing t.*thing_service ts`).
			WithArgs("thing-1").
			WillReturnRows(pgxmock.NewRows([]string{"management_url"}).AddRow(&url))
		store := New(mock)
		got, err := store.GetThingManagementURL(context.Background(), "thing-1")
		if err != nil || got != url {
			t.Errorf("got=%q err=%v", got, err)
		}
	})
	t.Run("thing exists no service row", func(t *testing.T) {
		// LEFT JOIN returns NULL management_url — Scan into *string =>
		// nil pointer, Get returns ("", nil).
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM thing t.*thing_service ts`).
			WithArgs("thing-1").
			WillReturnRows(pgxmock.NewRows([]string{"management_url"}).AddRow((*string)(nil)))
		store := New(mock)
		got, err := store.GetThingManagementURL(context.Background(), "thing-1")
		if err != nil || got != "" {
			t.Errorf("nil mgmt URL should return empty + nil; got %q %v", got, err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM thing t.*thing_service ts`).
			WithArgs("missing").
			WillReturnError(pgx.ErrNoRows)
		store := New(mock)
		_, err := store.GetThingManagementURL(context.Background(), "missing")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound; got: %v", err)
		}
	})
	t.Run("generic err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("planner err")
		mock.ExpectQuery(`FROM thing t.*thing_service ts`).
			WithArgs("x").
			WillReturnError(want)
		store := New(mock)
		_, err := store.GetThingManagementURL(context.Background(), "x")
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "get thing management url") {
			t.Errorf("missing prefix: %v", err)
		}
	})
}

// TestFindAgentByPhysicalID covers happy + empty-input + not-found
// + err.
func TestFindAgentByPhysicalID(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM thing.*physical_id = `).
			WithArgs("phys-1").
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("thing-found"))
		store := New(mock)
		got, err := store.FindAgentByPhysicalID(context.Background(), "phys-1")
		if err != nil || got != "thing-found" {
			t.Errorf("got=%q err=%v", got, err)
		}
	})
	t.Run("empty physicalID no-op", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		store := New(mock)
		got, err := store.FindAgentByPhysicalID(context.Background(), "")
		if err != nil || got != "" {
			t.Errorf("empty input: got=%q err=%v", got, err)
		}
	})
	t.Run("not found returns empty + nil", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM thing.*physical_id = `).
			WithArgs("missing").
			WillReturnError(pgx.ErrNoRows)
		store := New(mock)
		got, err := store.FindAgentByPhysicalID(context.Background(), "missing")
		if err != nil || got != "" {
			t.Errorf("not-found: got=%q err=%v", got, err)
		}
	})
	t.Run("err propagates", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("planner err")
		mock.ExpectQuery(`FROM thing.*physical_id = `).
			WithArgs("p").
			WillReturnError(want)
		store := New(mock)
		_, err := store.FindAgentByPhysicalID(context.Background(), "p")
		if !errors.Is(err, want) {
			t.Errorf("must propagate (not wrapped here): %v", err)
		}
	})
}

// TestUpdateLastSeen covers happy + not-found + err wrap.
func TestUpdateLastSeen(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing SET last_seen_at`).
			WithArgs("thing-1").
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		store := New(mock)
		if err := store.UpdateLastSeen(context.Background(), "thing-1"); err != nil {
			t.Fatalf("UpdateLastSeen: %v", err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing SET last_seen_at`).
			WithArgs("missing").
			WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
		store := New(mock)
		err := store.UpdateLastSeen(context.Background(), "missing")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound; got: %v", err)
		}
	})
	t.Run("exec err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("conn refused")
		mock.ExpectExec(`UPDATE thing SET last_seen_at`).
			WithArgs("x").
			WillReturnError(want)
		store := New(mock)
		err := store.UpdateLastSeen(context.Background(), "x")
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
	})
}

// TestRefreshLiveness covers happy + not-found + err wrap.
func TestRefreshLiveness(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing.*CASE WHEN status = 'offline'`).
			WithArgs("thing-1").
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		store := New(mock)
		if err := store.RefreshLiveness(context.Background(), "thing-1"); err != nil {
			t.Fatalf("RefreshLiveness: %v", err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing.*CASE WHEN status = 'offline'`).
			WithArgs("missing").
			WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
		store := New(mock)
		err := store.RefreshLiveness(context.Background(), "missing")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound; got: %v", err)
		}
	})
	t.Run("exec err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("planner err")
		mock.ExpectExec(`UPDATE thing.*CASE WHEN status = 'offline'`).
			WithArgs("x").
			WillReturnError(want)
		store := New(mock)
		err := store.RefreshLiveness(context.Background(), "x")
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
	})
}

// TestMarkOffline returns its raw exec err.
func TestMarkOffline(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing SET status = 'offline'`).
			WithArgs("thing-1").
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		store := New(mock)
		if err := store.MarkOffline(context.Background(), "thing-1"); err != nil {
			t.Fatalf("MarkOffline: %v", err)
		}
	})
	t.Run("exec err propagates", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("planner err")
		mock.ExpectExec(`UPDATE thing SET status = 'offline'`).
			WithArgs("x").
			WillReturnError(want)
		store := New(mock)
		err := store.MarkOffline(context.Background(), "x")
		if !errors.Is(err, want) {
			t.Errorf("must propagate (no wrap): %v", err)
		}
	})
}

// TestMarkStaleOffline returns RowsAffected.
func TestMarkStaleOffline(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing.*WHERE type = ANY`).
			WithArgs([]string{"agent", "ai-gateway"}, float64(300)).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 4"))
		store := New(mock)
		got, err := store.MarkStaleOffline(context.Background(),
			[]string{"agent", "ai-gateway"}, 5*time.Minute)
		if err != nil || got != 4 {
			t.Errorf("got=%d err=%v, want 4", got, err)
		}
	})
	t.Run("exec err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("planner err")
		mock.ExpectExec(`UPDATE thing.*WHERE type = ANY`).
			WithArgs([]string{"agent"}, float64(60)).
			WillReturnError(want)
		store := New(mock)
		_, err := store.MarkStaleOffline(context.Background(), []string{"agent"}, time.Minute)
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
	})
}

// TestUpdateThingAgent_HappyWithMirror covers the dual-update flow:
// non-empty Hostname/OS/OSVersion triggers the thing.* mirror update
// first, then the thing_agent upsert runs.
func TestUpdateThingAgent_HappyWithMirror(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectExec(`UPDATE thing\s+SET hostname`).
		WithArgs("thing-1", "host-1", "darwin", "14.0").
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
	mock.ExpectExec(`INSERT INTO thing_agent`).
		WithArgs("thing-1", "serial-abc", pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	expires := time.Now().Add(365 * 24 * time.Hour).UTC()
	store := New(mock)
	err := store.UpdateThingAgent(context.Background(), UpsertThingAgentParams{
		ThingID: "thing-1", Hostname: "host-1", OS: "darwin", OSVersion: "14.0",
		CertSerial: "serial-abc", CertExpiresAt: &expires,
	})
	if err != nil {
		t.Fatalf("UpdateThingAgent: %v", err)
	}
}

// TestUpdateThingAgent_NoMirrorWhenEmptyIdentity covers the branch
// where Hostname/OS/OSVersion are all empty — only the thing_agent
// upsert runs (no thing.* mirror).
func TestUpdateThingAgent_NoMirrorWhenEmptyIdentity(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// NO ExpectExec for the mirror UPDATE.
	mock.ExpectExec(`INSERT INTO thing_agent`).
		WithArgs("thing-1", "serial-abc", pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	expires := time.Now().Add(365 * 24 * time.Hour).UTC()
	store := New(mock)
	err := store.UpdateThingAgent(context.Background(), UpsertThingAgentParams{
		ThingID: "thing-1", CertSerial: "serial-abc", CertExpiresAt: &expires,
	})
	if err != nil {
		t.Fatalf("UpdateThingAgent: %v", err)
	}
}

// TestUpdateThingAgent_MirrorErrorWraps covers the mirror update
// err branch — wraps with "mirror thing identity:".
func TestUpdateThingAgent_MirrorErrorWraps(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	want := errors.New("constraint violation")
	mock.ExpectExec(`UPDATE thing\s+SET hostname`).
		WithArgs("thing-1", "host-1", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(want)

	store := New(mock)
	err := store.UpdateThingAgent(context.Background(), UpsertThingAgentParams{
		ThingID: "thing-1", Hostname: "host-1",
	})
	if !errors.Is(err, want) {
		t.Errorf("must wrap; got: %v", err)
	}
	if !strings.Contains(err.Error(), "mirror thing identity") {
		t.Errorf("missing prefix: %v", err)
	}
}

// TestUpdateThingAgent_UpsertErrorWraps covers the thing_agent
// INSERT err branch — wraps with "upsert thing_agent:".
func TestUpdateThingAgent_UpsertErrorWraps(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	want := errors.New("planner err")
	mock.ExpectExec(`INSERT INTO thing_agent`).
		WithArgs("thing-1", "serial", pgxmock.AnyArg()).
		WillReturnError(want)

	store := New(mock)
	err := store.UpdateThingAgent(context.Background(), UpsertThingAgentParams{
		ThingID: "thing-1", CertSerial: "serial",
	})
	if !errors.Is(err, want) {
		t.Errorf("must wrap; got: %v", err)
	}
	if !strings.Contains(err.Error(), "upsert thing_agent") {
		t.Errorf("missing prefix: %v", err)
	}
}

// TestValidateDeviceToken_HappyPath covers the lookup-by-hash flow
// returning the full Thing row.
func TestValidateDeviceToken_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	now := time.Now().UTC()
	// The regex requires the fail-closed expiry predicate to be present in the
	// SQL — if a future edit drops `device_token_expires_at > NOW()`, this
	// query no longer matches and ExpectationsWereMet fails (F-0202).
	mock.ExpectQuery(`metadata->>'deviceTokenHash' = \$2[\s\S]*device_token_expires_at > NOW\(\)`).
		WithArgs("thing-1", "hash-1").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "name", "version", "address",
			"enrolled_by", "auth_type", "conn_protocol",
			"status", "desired", "reported", "desired_ver", "reported_ver",
			"metadata", "last_seen_at", "enrolled_at",
			"reported_outcomes", "process_started_at",
		}).AddRow(
			"thing-1", "agent", "host", "1.0", "127.0.0.1:0",
			"sso", "bearer", "http",
			"online", []byte("{}"), []byte("{}"), int64(0), int64(0),
			[]byte(`{"deviceTokenHash":"hash-1"}`), &now, now,
			[]byte("{}"), (*time.Time)(nil),
		))

	store := New(mock)
	got, err := store.ValidateDeviceToken(context.Background(), "thing-1", "hash-1")
	if err != nil {
		t.Fatalf("ValidateDeviceToken: %v", err)
	}
	if got == nil || got.ID != "thing-1" || got.Status != "online" {
		t.Errorf("thing: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expiry predicate missing from query: %v", err)
	}
}

// TestValidateDeviceToken_ExpiredRejected proves the fail-closed expiry behaviour:
// when a token has expired, the `device_token_expires_at > NOW()` predicate drops
// the row server-side, so the lookup returns ErrNoRows and the agent is rejected
// — there is no Go branch that could let an expired token through (F-0202).
func TestValidateDeviceToken_ExpiredRejected(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// Mock returns no rows: that is exactly what Postgres does for an expired
	// (or NULL-expiry) token because of the WHERE predicate.
	mock.ExpectQuery(`device_token_expires_at > NOW\(\)`).
		WithArgs("thing-1", "stale-hash").
		WillReturnError(pgx.ErrNoRows)

	store := New(mock)
	_, err := store.ValidateDeviceToken(context.Background(), "thing-1", "stale-hash")
	if err == nil {
		t.Fatal("expired token must be rejected, got nil error")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("must wrap ErrNoRows (token filtered by expiry predicate); got: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expiry predicate missing from query: %v", err)
	}
}

// TestValidateDeviceToken_NoMatch covers the wrong-hash branch —
// the WHERE filter drops the row so QueryRow returns ErrNoRows;
// the function wraps with "validate device token:" (no special
// translation to ErrNotFound).
func TestValidateDeviceToken_NoMatch(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM thing\s+WHERE id =`).
		WithArgs("thing-1", "wrong-hash").
		WillReturnError(pgx.ErrNoRows)

	store := New(mock)
	_, err := store.ValidateDeviceToken(context.Background(), "thing-1", "wrong-hash")
	if err == nil {
		t.Fatal("expected wrap of ErrNoRows")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("must wrap ErrNoRows; got: %v", err)
	}
}

// TestStoreDeviceTokenHash covers happy + not-found + err wrap. The happy case
// also asserts the expiry column is written: the UPDATE must set
// device_token_expires_at to the supplied time (F-0202), bound as $3.
func TestStoreDeviceTokenHash(t *testing.T) {
	expiry := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing.*jsonb_set.*deviceTokenHash.*device_token_expires_at = \$3`).
			WithArgs("thing-1", "hash-abc", expiry).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		store := New(mock)
		if err := store.StoreDeviceTokenHash(context.Background(), "thing-1", "hash-abc", expiry); err != nil {
			t.Fatalf("StoreDeviceTokenHash: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("expiry column not written as expected: %v", err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing.*jsonb_set.*deviceTokenHash`).
			WithArgs("missing", "h", expiry).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
		store := New(mock)
		err := store.StoreDeviceTokenHash(context.Background(), "missing", "h", expiry)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound; got: %v", err)
		}
	})
	t.Run("exec err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("planner err")
		mock.ExpectExec(`UPDATE thing.*jsonb_set.*deviceTokenHash`).
			WithArgs("x", "h", expiry).
			WillReturnError(want)
		store := New(mock)
		err := store.StoreDeviceTokenHash(context.Background(), "x", "h", expiry)
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
	})
}

func TestStoreAttestationPubKey(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		exp := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		mock.ExpectExec(`UPDATE thing_agent.*jsonb_set.*attestation`).
			WithArgs("thing-1", pgxmock.AnyArg()).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		s := New(mock)
		if err := s.StoreAttestationPubKey(context.Background(), "thing-1",
			[]byte{0x01, 0x02, 0x03}, "FACE", exp); err != nil {
			t.Fatalf("StoreAttestationPubKey: %v", err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing_agent.*jsonb_set.*attestation`).
			WithArgs("missing", pgxmock.AnyArg()).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
		s := New(mock)
		err := s.StoreAttestationPubKey(context.Background(), "missing",
			[]byte{0x01}, "S", time.Now())
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound; got: %v", err)
		}
	})
	t.Run("db err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("planner err")
		mock.ExpectExec(`UPDATE thing_agent.*jsonb_set.*attestation`).
			WithArgs("x", pgxmock.AnyArg()).
			WillReturnError(want)
		s := New(mock)
		err := s.StoreAttestationPubKey(context.Background(), "x",
			[]byte{0x01}, "S", time.Now())
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
	})
}

// TestUpdateThingStatus covers happy + not-found + err wrap.
func TestUpdateThingStatus(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing SET status`).
			WithArgs("thing-1", "online").
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		store := New(mock)
		if err := store.UpdateThingStatus(context.Background(), "thing-1", "online"); err != nil {
			t.Fatalf("UpdateThingStatus: %v", err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing SET status`).
			WithArgs("missing", "offline").
			WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
		store := New(mock)
		err := store.UpdateThingStatus(context.Background(), "missing", "offline")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound; got: %v", err)
		}
	})
	t.Run("exec err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("planner err")
		mock.ExpectExec(`UPDATE thing SET status`).
			WithArgs("x", "s").
			WillReturnError(want)
		store := New(mock)
		err := store.UpdateThingStatus(context.Background(), "x", "s")
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
	})
}

// TestGetAttestationPubKeyWithExpiry covers the SEC-M4-01 getter that surfaces
// the cert NotAfter alongside the pubkey so CP can reject an expired key.
func TestGetAttestationPubKeyWithExpiry(t *testing.T) {
	t.Run("happy with expiry", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT COALESCE.*publicKey.*COALESCE.*certExpiresAt`).
			WithArgs("thing-1").
			WillReturnRows(pgxmock.NewRows([]string{"publicKey", "certExpiresAt"}).
				AddRow("AQID", "2099-01-02T03:04:05Z")) // base64 of [1,2,3]
		s := New(mock)
		pub, exp, err := s.GetAttestationPubKeyWithExpiry(context.Background(), "thing-1")
		if err != nil {
			t.Fatalf("GetAttestationPubKeyWithExpiry: %v", err)
		}
		if len(pub) != 3 || pub[0] != 0x01 {
			t.Errorf("pub = %v; want [1 2 3]", pub)
		}
		want, _ := time.Parse(time.RFC3339, "2099-01-02T03:04:05Z")
		if !exp.Equal(want) {
			t.Errorf("certExpiresAt = %v; want %v", exp, want)
		}
	})
	t.Run("legacy stamp without expiry returns zero time", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT COALESCE.*certExpiresAt`).
			WithArgs("thing-2").
			WillReturnRows(pgxmock.NewRows([]string{"publicKey", "certExpiresAt"}).AddRow("AQID", ""))
		s := New(mock)
		_, exp, err := s.GetAttestationPubKeyWithExpiry(context.Background(), "thing-2")
		if err != nil || !exp.IsZero() {
			t.Errorf("legacy: exp=%v err=%v; want zero,nil", exp, err)
		}
	})
	t.Run("empty publicKey is not-found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT COALESCE`).
			WithArgs("none").
			WillReturnRows(pgxmock.NewRows([]string{"publicKey", "certExpiresAt"}).AddRow("", ""))
		s := New(mock)
		if _, _, err := s.GetAttestationPubKeyWithExpiry(context.Background(), "none"); !errors.Is(err, ErrNotFound) {
			t.Errorf("want ErrNotFound; got %v", err)
		}
	})
	// SEC-M4-01 revocation: the query joins thing and excludes status='revoked',
	// so a revoked (unenrolled) device's attestation is no longer served — the
	// JOIN+filter yields no row (ErrNoRows) → ErrNotFound → CP MITM fallback.
	t.Run("revoked device not served", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		// The expected query MUST carry the revocation filter; pgxmock matches on
		// the regex, so this asserts the JOIN + status guard are present.
		mock.ExpectQuery(`JOIN thing t[\s\S]*status != 'revoked'`).
			WithArgs("revoked-thing").
			WillReturnError(pgx.ErrNoRows)
		s := New(mock)
		if _, _, err := s.GetAttestationPubKeyWithExpiry(context.Background(), "revoked-thing"); !errors.Is(err, ErrNotFound) {
			t.Errorf("revoked device must yield ErrNotFound; got %v", err)
		}
	})
}
