package iam

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A constraint the caller tripped must be reported as the caller's problem.
//
// This exists because prod answered `500 Failed to create user` for a create
// that was simply missing an organisation, and the field name — `organizationId`
// — appeared only in the server log. From outside, that is indistinguishable
// from the service being down, which is the difference between "fix your
// request" and "page someone".
func TestUserWriteConstraintError(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantInMsg  string
	}{
		{
			name:       "a missing required column names the column",
			err:        &pgconn.PgError{Code: "23502", ColumnName: "organizationId"},
			wantStatus: http.StatusBadRequest,
			wantCode:   "FIELD_REQUIRED",
			wantInMsg:  "organizationId",
		},
		{
			name: "a missing column with no name still says what kind of failure it is",
			err:  &pgconn.PgError{Code: "23502"},
			// Without a column name the message cannot be specific, but it must
			// still be a 400 — the caller can act on "you left something out",
			// and cannot act on "server error".
			wantStatus: http.StatusBadRequest,
			wantCode:   "FIELD_REQUIRED",
			wantInMsg:  "required field",
		},
		{
			name:       "a duplicate is a conflict, not a server fault",
			err:        &pgconn.PgError{Code: "23505", ConstraintName: "NexusUser_email_key"},
			wantStatus: http.StatusConflict,
			wantCode:   "USER_EXISTS",
		},
		{
			name:       "a dangling reference points at the field that carries it",
			err:        &pgconn.PgError{Code: "23503", ConstraintName: "NexusUser_organizationId_fkey"},
			wantStatus: http.StatusBadRequest,
			wantCode:   "REFERENCE_NOT_FOUND",
			wantInMsg:  "organizationId",
		},
		{
			// Wrapped, because the store returns errors through fmt.Errorf and
			// a type assertion instead of errors.As would miss every one of them.
			name:       "a wrapped constraint error is still classified",
			err:        fmt.Errorf("create user: %w", &pgconn.PgError{Code: "23502", ColumnName: "organizationId"}),
			wantStatus: http.StatusBadRequest,
			wantCode:   "FIELD_REQUIRED",
			wantInMsg:  "organizationId",
		},
		{
			// Everything the caller did NOT cause must stay a 500 — turning a
			// real outage into a 400 tells the operator to fix their request
			// while the database is on fire.
			name:       "a connection failure is not the caller's fault",
			err:        errors.New("connection refused"),
			wantStatus: 0,
		},
		{
			name:       "an unrelated pg error stays ours",
			err:        &pgconn.PgError{Code: "57014"}, // query_canceled
			wantStatus: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, code, status := userWriteConstraintError(tc.err)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d (msg=%q code=%q)", status, tc.wantStatus, msg, code)
			}
			if tc.wantStatus == 0 {
				return
			}
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
			if tc.wantInMsg != "" && !contains(msg, tc.wantInMsg) {
				t.Errorf("message = %q; want it to name %q so the caller knows what to change",
					msg, tc.wantInMsg)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
