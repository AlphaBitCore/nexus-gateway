package authstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

// TestTouchThingSession covers happy + not-found + exec err.
func TestTouchThingSession(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing SET`).
			WithArgs("thing-1", "1.2.3", "addr", "host").
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		s := New(mock)
		err := s.TouchThingSession(context.Background(), TouchSessionParams{
			ID: "thing-1", Version: "1.2.3", Address: "addr", Name: "host",
		})
		if err != nil {
			t.Fatalf("TouchThingSession: %v", err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing SET`).
			WithArgs("missing", "", "", "").
			WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
		s := New(mock)
		err := s.TouchThingSession(context.Background(), TouchSessionParams{ID: "missing"})
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound; got: %v", err)
		}
	})
	t.Run("exec err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("planner err")
		mock.ExpectExec(`UPDATE thing SET`).
			WithArgs("x", "", "", "").
			WillReturnError(want)
		s := New(mock)
		err := s.TouchThingSession(context.Background(), TouchSessionParams{ID: "x"})
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "touch thing session") {
			t.Errorf("missing prefix: %v", err)
		}
	})
}
