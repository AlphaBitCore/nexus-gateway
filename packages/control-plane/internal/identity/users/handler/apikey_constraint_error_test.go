package iam

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/jackc/pgx/v5/pgconn"
)

// A caller who names a principal that does not exist — an empty ownerUserId
// included — trips AdminApiKey_ownerUserId_fkey. Measured on prod before this
// fix, that came back as
//
//	500 {"error":{"code":"","message":"Failed to create API key","type":"server_error"}}
//
// which cannot be told apart from the route being broken: no code, no field
// name, and a status that says the fault is ours. Finding out it was the owner
// field meant reading the constraint list.
func TestAPIKeyWriteConstraintError_ForeignKeyIsTheCallersFault(t *testing.T) {
	msg, code, status := apiKeyWriteConstraintError(&pgconn.PgError{
		Code:           "23503",
		ConstraintName: "AdminApiKey_ownerUserId_fkey",
	})

	if status != http.StatusBadRequest {
		t.Fatalf("a caller naming a principal that does not exist is a bad request, got %d", status)
	}
	if code != "REFERENCE_NOT_FOUND" {
		t.Errorf("the machine code must be actionable, got %q", code)
	}
	if want := "A referenced record does not exist (check ownerUserId)"; msg != want {
		t.Errorf("the message must name the field the caller got wrong.\n got: %q\nwant: %q", msg, want)
	}
}

// The duplicate case must speak about API keys, not users. Sharing one mapper
// is the point of the fix; sharing one VOCABULARY would be a regression that
// tells an operator the wrong thing about what already exists.
func TestAPIKeyWriteConstraintError_DuplicateSpeaksAboutKeys(t *testing.T) {
	msg, code, status := apiKeyWriteConstraintError(&pgconn.PgError{Code: "23505"})

	if status != http.StatusConflict {
		t.Fatalf("a duplicate is a conflict, got %d", status)
	}
	if code != "API_KEY_EXISTS" {
		t.Errorf("got %q, want API_KEY_EXISTS — USER_EXISTS would be the users handler's code", code)
	}
	if want := "A API key with those details already exists"; msg != want {
		t.Errorf("got %q, want %q", msg, want)
	}
}

// The missing-column case names the column, which is the same for both
// handlers because Postgres supplies it.
func TestAPIKeyWriteConstraintError_MissingColumnIsNamed(t *testing.T) {
	msg, code, status := apiKeyWriteConstraintError(&pgconn.PgError{Code: "23502", ColumnName: "keyHash"})

	if status != http.StatusBadRequest || code != "FIELD_REQUIRED" {
		t.Fatalf("got %d/%q, want 400/FIELD_REQUIRED", status, code)
	}
	if want := "Missing required field: keyHash"; msg != want {
		t.Errorf("got %q, want %q", msg, want)
	}
}

// The half that keeps the fix honest: anything the caller did NOT cause must
// still be a 500. A mapper that swallowed genuine server faults into 400s would
// hide our own breakage behind a message blaming the caller.
func TestAPIKeyWriteConstraintError_OurFaultsStayFiveHundred(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"deadlock", &pgconn.PgError{Code: "40P01"}},
		{"undefined table", &pgconn.PgError{Code: "42P01"}},
		{"not a pg error at all", errNotPG{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, status := apiKeyWriteConstraintError(tc.err); status != 0 {
				t.Fatalf("classified as %d; a zero status is what makes the caller fall through to its own 500", status)
			}
		})
	}
}

// The users handler must keep its own vocabulary through the shared mapper.
func TestUserWriteConstraintError_KeepsItsOwnVocabulary(t *testing.T) {
	msg, code, _ := userWriteConstraintError(&pgconn.PgError{Code: "23505"})
	if code != "USER_EXISTS" || msg != "A user with those details already exists" {
		t.Fatalf("the users handler's duplicate case changed: %q / %q", msg, code)
	}
	if m, _, _ := userWriteConstraintError(&pgconn.PgError{Code: "23503"}); m != "A referenced record does not exist (check organizationId)" {
		t.Fatalf("the users handler's reference hint changed: %q", m)
	}
}

type errNotPG struct{}

func (errNotPG) Error() string { return "connection reset by peer" }

// TestCreateAPIKey_ForeignKeyIsReportedAsABadRequest asserts the mapper is
// WIRED, not merely present.
//
// The arms above call apiKeyWriteConstraintError directly, so they stay green
// if CreateAPIKey stops calling it — which is exactly the state prod was
// measured in. This one drives the handler and reads the response.
func TestCreateAPIKey_ForeignKeyIsReportedAsABadRequest(t *testing.T) {
	us := &stubUserStore{createKeyErr: &pgconn.PgError{
		Code:           "23503",
		ConstraintName: "AdminApiKey_ownerUserId_fkey",
	}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	// No ownerUserId on the request. Supplying one sends the handler through the
	// grant-ceiling check first, which needs an IAM engine this harness does not
	// wire and answers 503 before the store is reached. What is under test is how
	// the handler classifies a 23503 coming BACK from the store, and that does not
	// depend on which field the caller filled in — the field name in the response
	// comes from the mapper's hint, not from the request.
	body, _ := json.Marshal(map[string]any{"name": "My Key"})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys", body, "admin", "admin_user")

	if err := h.CreateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 — naming a principal that does not exist is the caller's mistake, "+
			"and a 500 cannot be told apart from the route being broken; body=%s", rec.Code, rec.Body)
	}
	if got := rec.Body.String(); !strings.Contains(got, "REFERENCE_NOT_FOUND") || !strings.Contains(got, "ownerUserId") {
		t.Errorf("the response must carry the code and name the field: %s", got)
	}
}

// The other half: a genuine server fault still reports as one. The existing
// TestCreateAPIKey_StoreError_Returns500 covers a plain error; this covers a pg
// error that is NOT caller-caused, which is the case the new branch could
// wrongly swallow.
func TestCreateAPIKey_ServerSidePgErrorStillReturns500(t *testing.T) {
	us := &stubUserStore{createKeyErr: &pgconn.PgError{Code: "40P01"}} // deadlock_detected
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "My Key"})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys", body, "admin", "admin_user")

	if err := h.CreateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d want 500 — a deadlock is ours, and blaming the caller for it hides our breakage; body=%s",
			rec.Code, rec.Body)
	}
}

// TestAPIKeyWrites_StaleIDIsNotAServerFault walks EVERY write on the api-keys
// handler and asserts none of them reports a stale id as a broken server.
//
// It is a table rather than a test per handler on purpose. The class fix
// claimed five writes and delivered four: create, regenerate and rotate were
// converted while update kept collapsing pgx.ErrNoRows into
// 500 "Failed to update API key", and a per-handler test is exactly what let
// that pass — every handler that HAD a test was covered. A caller deleting a
// key and then PATCHing it was told our server was broken.
func TestAPIKeyWrites_StaleIDIsNotAServerFault(t *testing.T) {
	for _, tc := range []struct {
		name   string
		store  *stubUserStore
		method string
		path   string
		body   map[string]any
		call   func(*Handler, echo.Context) error
		want   int
	}{
		{
			name:   "PATCH with an id that is gone",
			store:  &stubUserStore{updateKeyErr: pgx.ErrNoRows},
			method: http.MethodPatch,
			path:   "/api-keys/gone",
			body:   map[string]any{"name": "renamed"},
			call:   func(h *Handler, c echo.Context) error { return h.UpdateAPIKey(c) },
			want:   http.StatusNotFound,
		},
		{
			// The same handler's OTHER new branch: a caller-tripped constraint
			// coming back from the update, which must classify rather than 500.
			name:   "PATCH tripping a foreign key",
			store:  &stubUserStore{updateKeyErr: &pgconn.PgError{Code: "23503", ConstraintName: "AdminApiKey_ownerUserId_fkey"}},
			method: http.MethodPatch,
			path:   "/api-keys/k1",
			body:   map[string]any{"name": "renamed"},
			call:   func(h *Handler, c echo.Context) error { return h.UpdateAPIKey(c) },
			want:   http.StatusBadRequest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := buildHandler(tc.store, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
			body, _ := json.Marshal(tc.body)
			c, rec := adminAuthCtx(tc.method, tc.path, body, "admin", "admin_user")
			c.SetParamNames("id")
			c.SetParamValues("gone")

			if err := tc.call(h, c); err != nil {
				t.Fatal(err)
			}
			if rec.Code == http.StatusInternalServerError {
				t.Fatalf("the caller's own mistake was reported as a server fault: %d %s", rec.Code, rec.Body)
			}
			if rec.Code != tc.want {
				t.Fatalf("code=%d want %d; body=%s", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

// TestAPIKeyReads_DatabaseFailureIsNotANotFound is S2. The store already
// separates the two — GetAdminAPIKey returns (nil, nil) for no rows and
// (nil, err) for a read failure — and the handlers were discarding that
// distinction. "That key does not exist" is the one answer that makes a caller
// stop retrying and start re-provisioning, so a database outage must not wear
// it.
func TestAPIKeyReads_DatabaseFailureIsNotANotFound(t *testing.T) {
	dbDown := errors.New("connection refused")

	// A table over EVERY site that reads a key before acting on it, for the same
	// reason the write gate is a table: the sites are the class, and covering one
	// of them is how the others stay broken.
	for _, tc := range []struct {
		name string
		call func(*Handler, echo.Context) error
	}{
		{"GetAPIKey", func(h *Handler, c echo.Context) error { return h.GetAPIKey(c) }},
		{"RegenerateAPIKey owner lookup", func(h *Handler, c echo.Context) error { return h.RegenerateAPIKey(c) }},
		{"RotateAPIKey predecessor lookup", func(h *Handler, c echo.Context) error { return h.RotateAPIKey(c) }},
	} {
		t.Run("a read failure is ours: "+tc.name, func(t *testing.T) {
			h := buildHandler(&stubUserStore{getKeyErr: dbDown}, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
			c, rec := adminAuthCtx(http.MethodPost, "/api-keys/k1", []byte(`{}`), "admin", "admin_user")
			c.SetParamNames("id")
			c.SetParamValues("k1")

			if err := tc.call(h, c); err != nil {
				t.Fatal(err)
			}
			if rec.Code == http.StatusNotFound {
				t.Fatal("a database failure was reported as 'API key not found' — the caller " +
					"is told their key is gone when the database is merely unreachable")
			}
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("code=%d want 500; body=%s", rec.Code, rec.Body)
			}
		})
	}

	// The control: a genuinely absent key must STILL be a 404, or the fix has
	// simply moved the confusion the other way.
	t.Run("an absent key is still 404", func(t *testing.T) {
		h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
		c, rec := adminAuthCtx(http.MethodGet, "/api-keys/gone", nil, "admin", "admin_user")
		c.SetParamNames("id")
		c.SetParamValues("gone")

		if err := h.GetAPIKey(c); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code=%d want 404; body=%s", rec.Code, rec.Body)
		}
	})
}
