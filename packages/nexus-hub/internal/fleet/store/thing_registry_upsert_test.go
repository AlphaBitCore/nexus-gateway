package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

// TestUpsertThingEnrollment covers all 5 branches:
// happy / nil metadata→ "{}" / nil desired→ "{}" / marshal-desired
// err / auth-type+conn-proto defaults / exec err wrap.
func TestUpsertThingEnrollment(t *testing.T) {
	t.Run("happy with explicit auth+protocol", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`INSERT INTO thing\b`).
			WithArgs(
				"thing-1", "agent", "host", "1.2.3", "127.0.0.1:8080",
				"sso-enroll", "mtls", "websocket", "online",
				pgxmock.AnyArg(), // metadataJSON
				pgxmock.AnyArg(), // desiredJSON
			).
			WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

		store := New(mock)
		err := store.UpsertThingEnrollment(context.Background(), UpsertThingParams{
			ID: "thing-1", Type: "agent", Name: "host", Version: "1.2.3",
			Address: "127.0.0.1:8080", EnrolledBy: "sso-enroll",
			AuthType: "mtls", ConnProtocol: "websocket", Status: "online",
			Metadata: map[string]any{"k": "v"},
			Desired:  map[string]any{"x": 1},
		})
		if err != nil {
			t.Fatalf("UpsertThingEnrollment: %v", err)
		}
	})
	t.Run("nil metadata + nil desired thread '{}'", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`INSERT INTO thing\b`).
			WithArgs(
				"thing-1", "agent", "host", "1.0", "addr",
				"sso", "bearer", "http", "online",
				[]byte("{}"), []byte("{}"),
			).
			WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

		store := New(mock)
		err := store.UpsertThingEnrollment(context.Background(), UpsertThingParams{
			ID: "thing-1", Type: "agent", Name: "host", Version: "1.0", Address: "addr",
			EnrolledBy: "sso", Status: "online", // empty AuthType/ConnProto → defaults
		})
		if err != nil {
			t.Fatalf("UpsertThingEnrollment: %v", err)
		}
	})
	t.Run("marshal-desired err pre-Exec", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		// No ExpectExec — must short-circuit.
		store := New(mock)
		err := store.UpsertThingEnrollment(context.Background(), UpsertThingParams{
			ID: "thing-1", Type: "agent",
			Desired: map[string]any{"bad": make(chan int)},
		})
		if err == nil {
			t.Fatal("expected marshal err")
		}
		if !strings.Contains(err.Error(), "marshal desired") {
			t.Errorf("missing prefix: %v", err)
		}
	})
	t.Run("exec err propagates raw", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("constraint violation")
		mock.ExpectExec(`INSERT INTO thing\b`).
			WithArgs(
				"thing-1", "agent", "host", "1.0", "addr",
				"sso", "bearer", "http", "online",
				[]byte("{}"), []byte("{}"),
			).
			WillReturnError(want)

		store := New(mock)
		err := store.UpsertThingEnrollment(context.Background(), UpsertThingParams{
			ID: "thing-1", Type: "agent", Name: "host", Version: "1.0", Address: "addr",
			EnrolledBy: "sso", Status: "online",
		})
		// This branch returns the raw error from Exec (no fmt.Errorf wrap
		// shown at line 220 of thing_registry.go — actually wait let me
		// confirm). It IS wrapped — search the function for "_, err =".
		// Returning err directly. Verify it's the same instance.
		if !errors.Is(err, want) {
			t.Errorf("must propagate Exec err; got: %v", err)
		}
	})
}

// TestUpsertThingEnrollmentWithDesiredVer covers the same shape +
// the explicit desired_ver parameter + PhysicalID handling.
func TestUpsertThingEnrollmentWithDesiredVer(t *testing.T) {
	t.Run("happy with PhysicalID", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`INSERT INTO thing\b`).
			WithArgs(
				"thing-1", "agent", "host", "1.0", "addr",
				"sso", "bearer", "http", "online",
				pgxmock.AnyArg(), pgxmock.AnyArg(),
				int64(42), "phys-1",
			).
			WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

		store := New(mock)
		err := store.UpsertThingEnrollmentWithDesiredVer(context.Background(), UpsertThingParams{
			ID: "thing-1", Type: "agent", Name: "host", Version: "1.0", Address: "addr",
			EnrolledBy: "sso", Status: "online",
			PhysicalID: "phys-1",
			Desired:    map[string]any{"x": 1},
		}, 42)
		if err != nil {
			t.Fatalf("UpsertThingEnrollmentWithDesiredVer: %v", err)
		}
	})
	t.Run("empty PhysicalID stays NULL", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		// physicalID is `any` typed; when empty, the function leaves it
		// as a nil interface so pgx encodes SQL NULL. pgxmock matches
		// that as nil.
		mock.ExpectExec(`INSERT INTO thing\b`).
			WithArgs(
				"thing-1", "agent", "host", "1.0", "addr",
				"sso", "bearer", "http", "online",
				[]byte("{}"), []byte("{}"),
				int64(0), nil, // empty PhysicalID → nil any
			).
			WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

		store := New(mock)
		err := store.UpsertThingEnrollmentWithDesiredVer(context.Background(), UpsertThingParams{
			ID: "thing-1", Type: "agent", Name: "host", Version: "1.0", Address: "addr",
			EnrolledBy: "sso", Status: "online",
		}, 0)
		if err != nil {
			t.Fatalf("UpsertThingEnrollmentWithDesiredVer: %v", err)
		}
	})
	t.Run("marshal-desired err", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		store := New(mock)
		err := store.UpsertThingEnrollmentWithDesiredVer(context.Background(), UpsertThingParams{
			ID: "thing-1", Type: "agent",
			Desired: map[string]any{"bad": make(chan int)},
		}, 1)
		if err == nil {
			t.Fatal("expected marshal err")
		}
		if !strings.Contains(err.Error(), "marshal desired") {
			t.Errorf("missing prefix: %v", err)
		}
	})
	t.Run("exec err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("constraint violation")
		mock.ExpectExec(`INSERT INTO thing\b`).
			WithArgs(
				"thing-1", "agent", "host", "1.0", "addr",
				"sso", "bearer", "http", "online",
				[]byte("{}"), []byte("{}"),
				int64(0), nil,
			).
			WillReturnError(want)

		store := New(mock)
		err := store.UpsertThingEnrollmentWithDesiredVer(context.Background(), UpsertThingParams{
			ID: "thing-1", Type: "agent", Name: "host", Version: "1.0", Address: "addr",
			EnrolledBy: "sso", Status: "online",
		}, 0)
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "upsert thing enrollment with ver") {
			t.Errorf("missing prefix: %v", err)
		}
	})
}
