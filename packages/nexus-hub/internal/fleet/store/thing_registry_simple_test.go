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

// TestSetPhysicalID covers happy + empty-args no-op + exec err.
func TestSetPhysicalID(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing.*physical_id`).
			WithArgs("thing-1", "phys-1").
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		store := New(mock)
		if err := store.SetPhysicalID(context.Background(), "thing-1", "phys-1"); err != nil {
			t.Fatalf("SetPhysicalID: %v", err)
		}
	})
	t.Run("empty thingID no-op", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		store := New(mock)
		if err := store.SetPhysicalID(context.Background(), "", "phys"); err != nil {
			t.Errorf("empty thingID should no-op; got: %v", err)
		}
	})
	t.Run("empty physicalID no-op", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		store := New(mock)
		if err := store.SetPhysicalID(context.Background(), "t", ""); err != nil {
			t.Errorf("empty physicalID should no-op; got: %v", err)
		}
	})
	t.Run("exec err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("planner err")
		mock.ExpectExec(`UPDATE thing.*physical_id`).
			WithArgs("t", "p").
			WillReturnError(want)
		store := New(mock)
		err := store.SetPhysicalID(context.Background(), "t", "p")
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
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
	mock.ExpectQuery(`FROM thing\s+WHERE id =`).
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

// TestStoreDeviceTokenHash covers happy + not-found + err wrap.
func TestStoreDeviceTokenHash(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing.*jsonb_set.*deviceTokenHash`).
			WithArgs("thing-1", "hash-abc").
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		store := New(mock)
		if err := store.StoreDeviceTokenHash(context.Background(), "thing-1", "hash-abc"); err != nil {
			t.Fatalf("StoreDeviceTokenHash: %v", err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing.*jsonb_set.*deviceTokenHash`).
			WithArgs("missing", "h").
			WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
		store := New(mock)
		err := store.StoreDeviceTokenHash(context.Background(), "missing", "h")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound; got: %v", err)
		}
	})
	t.Run("exec err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("planner err")
		mock.ExpectExec(`UPDATE thing.*jsonb_set.*deviceTokenHash`).
			WithArgs("x", "h").
			WillReturnError(want)
		store := New(mock)
		err := store.StoreDeviceTokenHash(context.Background(), "x", "h")
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

func TestGetAttestationPubKey(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT COALESCE.*attestation`).
			WithArgs("thing-1").
			WillReturnRows(pgxmock.NewRows([]string{"coalesce"}).
				AddRow("AQID")) // base64 of [0x01, 0x02, 0x03]
		s := New(mock)
		got, err := s.GetAttestationPubKey(context.Background(), "thing-1")
		if err != nil {
			t.Fatalf("GetAttestationPubKey: %v", err)
		}
		want := []byte{0x01, 0x02, 0x03}
		if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Errorf("bytes mismatch: got=%v want=%v", got, want)
		}
	})
	t.Run("empty row → ErrNotFound", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT COALESCE.*attestation`).
			WithArgs("none").
			WillReturnRows(pgxmock.NewRows([]string{"coalesce"}).AddRow(""))
		s := New(mock)
		_, err := s.GetAttestationPubKey(context.Background(), "none")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound; got: %v", err)
		}
	})
	t.Run("pgx ErrNoRows → ErrNotFound", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT COALESCE.*attestation`).
			WithArgs("missing-thing").
			WillReturnError(pgx.ErrNoRows)
		s := New(mock)
		_, err := s.GetAttestationPubKey(context.Background(), "missing-thing")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound; got: %v", err)
		}
	})
	t.Run("query error wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("planner err")
		mock.ExpectQuery(`SELECT COALESCE.*attestation`).
			WithArgs("x").
			WillReturnError(want)
		s := New(mock)
		_, err := s.GetAttestationPubKey(context.Background(), "x")
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
	})
	t.Run("malformed base64 wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT COALESCE.*attestation`).
			WithArgs("bad").
			WillReturnRows(pgxmock.NewRows([]string{"coalesce"}).AddRow("!!!"))
		s := New(mock)
		_, err := s.GetAttestationPubKey(context.Background(), "bad")
		if err == nil {
			t.Fatal("expected base64 decode error")
		}
		if !strings.Contains(err.Error(), "decode attestation pubkey") {
			t.Errorf("err should wrap decode: %v", err)
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
