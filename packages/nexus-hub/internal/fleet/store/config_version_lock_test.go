package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

// TestAcquireConfigVersionLock asserts the per-type advisory lock that
// serializes desired_ver allocation (F-0109): the exact SQL form, the type key
// argument, and the error wrap. The distinct-version OUTCOME this lock
// guarantees is exercised end-to-end against real Postgres in the manager
// package's TestManager_UpdateConfig_DistinctVersionsUnderConcurrency.
func TestAcquireConfigVersionLock(t *testing.T) {
	t.Run("issues per-type advisory lock with the type key", func(t *testing.T) {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer mock.Close()
		mock.ExpectBegin()
		// The lock must hash the Thing TYPE (not a constant) so different types
		// allocate versions independently while same-type writers serialize.
		mock.ExpectExec(`SELECT pg_advisory_xact_lock\(hashtextextended\(\$1, 0\)\)`).
			WithArgs("agent").
			WillReturnResult(pgconn.NewCommandTag("SELECT 1"))

		tx, err := mock.Begin(context.Background())
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := New(mock).AcquireConfigVersionLock(context.Background(), tx, "agent"); err != nil {
			t.Fatalf("AcquireConfigVersionLock: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}
	})

	t.Run("wraps exec error with the type for diagnosis", func(t *testing.T) {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer mock.Close()
		mock.ExpectBegin()
		mock.ExpectExec(`pg_advisory_xact_lock`).
			WithArgs("compliance-proxy").
			WillReturnError(errors.New("lock timeout"))

		tx, err := mock.Begin(context.Background())
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		err = New(mock).AcquireConfigVersionLock(context.Background(), tx, "compliance-proxy")
		if err == nil || !strings.Contains(err.Error(), "acquire config version lock for compliance-proxy") {
			t.Errorf("err = %v, want wrapped lock error naming the type", err)
		}
	})
}
