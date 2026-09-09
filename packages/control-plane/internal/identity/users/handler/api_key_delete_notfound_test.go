package iam

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
)

// deleteKeyStub embeds the store interface so it satisfies the handler's wide
// dependency while implementing only the one method under test. Any other call
// would nil-panic, which is the point: this test must exercise DeleteAPIKey and
// nothing else.
type deleteKeyStub struct {
	iamUserStore
	err error
}

func (d deleteKeyStub) DeleteAdminAPIKey(ctx context.Context, id string) error { return d.err }

// Deleting a key that is already gone is the caller's stale id, not our outage.
//
// Found on prod while cleaning up a verification fixture: deleting the owning
// USER cascades the key away, and the follow-up DELETE on the key answered
// HTTP 500. The store had already distinguished the two cases — it returns
// pgx.ErrNoRows when nothing matched — and the handler collapsed that into a
// server fault, discarding an answer it had been handed.
//
// The distinction is what a caller acts on: 404 means fix your id, 500 means
// page someone.
func TestDeleteAPIKey_GoneIsNotFoundNotServerError(t *testing.T) {
	for _, tc := range []struct {
		name       string
		storeErr   error
		wantStatus int
		wantType   string
	}{
		{
			name:       "a key that is already gone",
			storeErr:   pgx.ErrNoRows,
			wantStatus: http.StatusNotFound,
			wantType:   "not_found",
		},
		{
			// Wrapped, because a store that adds context through fmt.Errorf
			// would otherwise slip past a bare equality check.
			name:       "a wrapped no-rows",
			storeErr:   errors.Join(errors.New("delete admin api key"), pgx.ErrNoRows),
			wantStatus: http.StatusNotFound,
			wantType:   "not_found",
		},
		{
			// A real fault must NOT be softened into a 404 — that would tell an
			// operator their id is wrong while the database is unreachable.
			name:       "the database is unreachable",
			storeErr:   errors.New("connection refused"),
			wantStatus: http.StatusInternalServerError,
			wantType:   "server_error",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			req := httptest.NewRequest(http.MethodDelete, "/api/admin/api-keys/k1", nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.SetParamNames("id")
			c.SetParamValues("k1")

			h := &Handler{users: deleteKeyStub{err: tc.storeErr}}
			if err := h.DeleteAPIKey(c); err != nil {
				t.Fatalf("DeleteAPIKey returned err: %v", err)
			}
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			var env struct {
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("body did not parse: %v (%s)", err, rec.Body.String())
			}
			if env.Error.Type != tc.wantType {
				t.Errorf("error.type = %q, want %q — the type is what a client branches on",
					env.Error.Type, tc.wantType)
			}
		})
	}
}
