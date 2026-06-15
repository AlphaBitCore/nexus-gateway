package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

// TestNew_BadDSN exercises the parse-config error branch of New(). The
// success branch needs a live Postgres so is left to integration tests.
func TestNew_BadDSN(t *testing.T) {
	_, err := New(context.Background(), "not-a-valid-dsn://!@#$")
	if err == nil {
		t.Fatal("expected parse-config error")
	}
	if !strings.Contains(err.Error(), "store:") {
		t.Errorf("missing wrap prefix: %v", err)
	}
}

// TestNew_PoolConfigAppliedThenPingFails drives the PoolConfig tuning path:
// a well-formed DSN to a dead port means ParseConfig + NewWithConfig succeed
// (pgxpool connects lazily), the non-zero MaxConns/MinConns/MaxConnLifetime
// overrides are applied to the config, and then Ping fails fast — so New
// returns the ping error with the wrap prefix. Exercises every PoolConfig
// branch (the live-DB happy return still needs an integration DB).
func TestNew_PoolConfigAppliedThenPingFails(t *testing.T) {
	dsn := "postgres://u:p@127.0.0.1:1/db?connect_timeout=1&sslmode=disable"
	opts := PoolConfig{MaxConns: 4, MinConns: 1, MaxConnLifetime: time.Minute}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := New(ctx, dsn, opts)
	if err == nil {
		if db != nil {
			db.Close()
		}
		t.Fatal("Ping against a dead port must return an error")
	}
	if !strings.Contains(err.Error(), "store:") {
		t.Errorf("missing wrap prefix: %v", err)
	}
}

func TestNewWithPgxPool(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	db := NewWithPgxPool(mock)
	if db == nil {
		t.Fatal("nil db")
	}
	if db.Pool != nil {
		t.Errorf("test constructor must leave Pool nil; got %v", db.Pool)
	}
}

func TestGetSystemMetadata(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
			WithArgs("foo").
			WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(`{"x":1}`)))
		got, err := db.GetSystemMetadata(context.Background(), "foo")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if string(got) != `{"x":1}` {
			t.Errorf("unexpected: %s", string(got))
		}
	})

	t.Run("no rows returns (nil,nil)", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM system_metadata`).
			WithArgs("missing").
			WillReturnError(pgx.ErrNoRows)
		got, err := db.GetSystemMetadata(context.Background(), "missing")
		if err != nil || got != nil {
			t.Errorf("want (nil,nil); got %v %v", got, err)
		}
	})

	t.Run("err wraps", func(t *testing.T) {
		mock, db := newMockDB(t)
		want := errors.New("planner err")
		mock.ExpectQuery(`FROM system_metadata`).
			WithArgs("k").
			WillReturnError(want)
		_, err := db.GetSystemMetadata(context.Background(), "k")
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), `get system metadata "k"`) {
			t.Errorf("missing prefix: %v", err)
		}
	})
}
