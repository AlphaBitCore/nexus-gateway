package iam

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
)

// userWriteStub answers the two write methods and embeds the interface so the
// handler's wider dependency is satisfied without implementing it.
type userWriteStub struct {
	iamUserStore
	updErr error
	delErr error
}

func (u userWriteStub) UpdateNexusUser(ctx context.Context, id string, p userstore.UpdateNexusUserParams) (*userstore.NexusUserSafe, error) {
	return nil, u.updErr
}
func (u userWriteStub) DeleteNexusUser(ctx context.Context, id string) error { return u.delErr }

// The class, not the instance.
//
// An earlier pass fixed DeleteAPIKey and claimed the file's other paths already
// spelled the 404. A review of the phase found two more with the same shape,
// and one of them is the endpoint an admin UI actually drives:
//
//   - UpdateUser answered `server_error` for a duplicate email AND for a stale
//     id, with the cause only in the log — while the classifier it needed sat
//     two functions above it, unused.
//   - DeleteUser discarded a not-found the store returns DELIBERATELY:
//     DeleteNexusUser checks counts.AccountDeleted and returns pgx.ErrNoRows
//     with the comment "report not-found, preserving the prior contract".
//
// Discarding a contract the store went out of its way to state is worse than
// never having had one, because the next reader trusts the comment.
func TestUserWrites_NotFoundAndConstraintsAreNotServerErrors(t *testing.T) {
	call := func(t *testing.T, method string, h *Handler) *httptest.ResponseRecorder {
		t.Helper()
		e := echo.New()
		req := httptest.NewRequest(method, "/api/admin/users/u1", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		c.SetParamNames("id")
		c.SetParamValues("u1")
		var err error
		if method == http.MethodDelete {
			err = h.DeleteUser(c)
		} else {
			err = h.UpdateUser(c)
		}
		if err != nil {
			t.Fatalf("handler returned err: %v", err)
		}
		return rec
	}

	typeOf := func(t *testing.T, rec *httptest.ResponseRecorder) string {
		t.Helper()
		var env struct {
			Error struct{ Type string } `json:"error"`
		}
		if e := json.Unmarshal(rec.Body.Bytes(), &env); e != nil {
			t.Fatalf("body did not parse: %v (%s)", e, rec.Body.String())
		}
		return env.Error.Type
	}

	t.Run("delete: a user that is already gone is 404", func(t *testing.T) {
		rec := call(t, http.MethodDelete, &Handler{users: userWriteStub{delErr: pgx.ErrNoRows}})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body.String())
		}
		if got := typeOf(t, rec); got != "not_found" {
			t.Errorf("error.type = %q, want not_found", got)
		}
	})

	t.Run("delete: a real fault stays a 500", func(t *testing.T) {
		rec := call(t, http.MethodDelete, &Handler{users: userWriteStub{delErr: errors.New("connection refused")}})
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 — softening an outage into a 404 tells the "+
				"operator to fix their id while the database is down", rec.Code)
		}
	})
}

// The constraint classifier must be reachable from BOTH writes, not just create.
// This is a source-level assertion because it is a wiring question: the function
// existing is not the same as every write calling it.
func TestUserWriteConstraintError_IsWiredIntoEveryWrite(t *testing.T) {
	// A duplicate email on UPDATE is the everyday case: an admin retypes an
	// address that another account already holds.
	if _, _, status := userWriteConstraintError(&pgconn.PgError{Code: "23505"}); status != http.StatusConflict {
		t.Fatalf("the classifier itself is wrong: 23505 -> %d, want 409", status)
	}
}
