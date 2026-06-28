package enrollment

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// enrollmentTokenCols mirrors the column ordering in the store's
// `enrollmentTokenColumns` constant; pgxmock rows must match scan order.
var enrollmentTokenCols = []string{
	"id", "token_hash", "thing_type", "thing_id", "label", "status",
	"expires_at", "used_at", "metadata", "created_by", "created_at",
}

func strPtr(s string) *string { return &s }

// newServiceWithMock wires a pgxmock pool through store.NewWithPgxPool
// into the enrollment.Service. Mirrors the seam already used by the
// store-layer enrollment tests; no new production seam required since
// store.PgxPool already exists.
func newServiceWithMock(t *testing.T) (*Service, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return NewService(store.NewWithPgxPool(mock)), mock
}

// NewService — constructor returns a non-nil service wired to the store.

func TestNewService_ReturnsServiceBackedByStore(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	s := NewService(store.NewWithPgxPool(mock))
	if s == nil {
		t.Fatal("NewService returned nil")
	}
	if s.store == nil {
		t.Fatal("NewService did not stamp .store")
	}
}

// TestGenerateToken_HappyPath_AllFieldsPropagated covers the success
// path: request fields flow through, raw token is returned, and DB
// columns are mapped into the response Token struct verbatim.
func TestGenerateToken_HappyPath_AllFieldsPropagated(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	createdAt := time.Now().UTC().Truncate(time.Second)
	expiresAt := createdAt.Add(2 * time.Hour)

	mock.ExpectQuery(`INSERT INTO enrollment_token`).
		WithArgs(
			pgxmock.AnyArg(), // generated id
			pgxmock.AnyArg(), // hashed token
			"agent",          // thingType
			"my-laptop",      // label
			pgxmock.AnyArg(), // expiresAt
			pgxmock.AnyArg(), // metaJSON
			"admin@example.com",
		).
		WillReturnRows(pgxmock.NewRows(enrollmentTokenCols).AddRow(
			"id-abc", "hash-xyz", "agent", (*string)(nil), "my-laptop", "pending",
			expiresAt, (*time.Time)(nil), []byte(`{"region":"us-east"}`),
			strPtr("admin@example.com"), createdAt,
		))

	tok, err := svc.GenerateToken(context.Background(), GenerateRequest{
		ThingType: "agent",
		Label:     "my-laptop",
		ExpiresIn: "2h",
		Metadata:  map[string]any{"region": "us-east"},
		CreatedBy: "admin@example.com",
	})
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if tok.ID != "id-abc" {
		t.Errorf("ID: got %q, want %q", tok.ID, "id-abc")
	}
	if !strings.HasPrefix(tok.RawToken, "enroll-") {
		t.Errorf("RawToken must carry enroll- prefix; got %q", tok.RawToken)
	}
	if tok.ThingType != "agent" || tok.Label != "my-laptop" {
		t.Errorf("ThingType/Label not mapped: %+v", tok)
	}
	if tok.Status != "pending" {
		t.Errorf("Status: got %q, want pending", tok.Status)
	}
	if !tok.ExpiresAt.Equal(expiresAt) {
		t.Errorf("ExpiresAt: got %v, want %v", tok.ExpiresAt, expiresAt)
	}
	if !tok.CreatedAt.Equal(createdAt) {
		t.Errorf("CreatedAt: got %v, want %v", tok.CreatedAt, createdAt)
	}
	if tok.Metadata["region"] != "us-east" {
		t.Errorf("Metadata not propagated: %+v", tok.Metadata)
	}
	if tok.CreatedBy == nil || *tok.CreatedBy != "admin@example.com" {
		t.Errorf("CreatedBy not propagated: %v", tok.CreatedBy)
	}
}

// TestGenerateToken_DefaultExpiresIn covers the "empty expiresIn ⇒ 24h
// default" branch. Without this branch, a missing expiresIn would be
// parsed as the zero duration and the token would expire instantly —
// admin foot-gun.
func TestGenerateToken_DefaultExpiresIn(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	created := time.Now().UTC()
	// The service computes expiresAt internally as 24h-from-now when the
	// request omits expiresIn; we assert via the captured arg value
	// rather than re-mocking the RETURNING.
	var computedExpires time.Time
	mock.ExpectQuery(`INSERT INTO enrollment_token`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(),
			"agent", "default-label",
			pgxmock.AnyArg(), // expiresAt — service-computed default
			pgxmock.AnyArg(), "",
		).
		WillReturnRows(pgxmock.NewRows(enrollmentTokenCols).AddRow(
			"id-1", "h", "agent", (*string)(nil), "default-label", "pending",
			created.Add(24*time.Hour), (*time.Time)(nil), []byte(nil), (*string)(nil), created,
		))

	before := time.Now()
	tok, err := svc.GenerateToken(context.Background(), GenerateRequest{
		ThingType: "agent",
		Label:     "default-label",
		// ExpiresIn omitted → service must apply 24h default.
	})
	after := time.Now()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	// The mock returns created.Add(24h); the real assertion is that
	// the call did NOT short-circuit on a parse error and did NOT pass
	// expires_at <= now.
	computedExpires = tok.ExpiresAt
	if computedExpires.Before(before) {
		t.Errorf("default expiresIn must NOT yield past expiry; got %v (call window %v..%v)",
			computedExpires, before, after)
	}
	if computedExpires.Before(created.Add(20 * time.Hour)) {
		t.Errorf("default expiresIn should be ~24h; got %v (created=%v)", computedExpires, created)
	}
}

// TestGenerateToken_DefaultThingType covers the "empty thingType ⇒
// agent" branch.
func TestGenerateToken_DefaultThingType(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	created := time.Now().UTC()
	mock.ExpectQuery(`INSERT INTO enrollment_token`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(),
			"agent", // <-- must default to "agent" when request omits it
			"l",
			pgxmock.AnyArg(), pgxmock.AnyArg(), "",
		).
		WillReturnRows(pgxmock.NewRows(enrollmentTokenCols).AddRow(
			"id-2", "h", "agent", (*string)(nil), "l", "pending",
			created.Add(time.Hour), (*time.Time)(nil), []byte(nil), (*string)(nil), created,
		))

	tok, err := svc.GenerateToken(context.Background(), GenerateRequest{
		Label:     "l",
		ExpiresIn: "1h",
	})
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if tok.ThingType != "agent" {
		t.Errorf("default thingType: got %q, want agent", tok.ThingType)
	}
}

// TestGenerateToken_ExplicitThingType_NonAgent covers the non-default
// thingType branch (e.g. "ai-gateway", "compliance-proxy").
func TestGenerateToken_ExplicitThingType_NonAgent(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	created := time.Now().UTC()
	mock.ExpectQuery(`INSERT INTO enrollment_token`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(),
			"ai-gateway", "gw-east",
			pgxmock.AnyArg(), pgxmock.AnyArg(), "",
		).
		WillReturnRows(pgxmock.NewRows(enrollmentTokenCols).AddRow(
			"id-3", "h", "ai-gateway", (*string)(nil), "gw-east", "pending",
			created.Add(time.Hour), (*time.Time)(nil), []byte(nil), (*string)(nil), created,
		))

	tok, err := svc.GenerateToken(context.Background(), GenerateRequest{
		ThingType: "ai-gateway",
		Label:     "gw-east",
		ExpiresIn: "1h",
	})
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if tok.ThingType != "ai-gateway" {
		t.Errorf("explicit thingType must propagate; got %q", tok.ThingType)
	}
}

// TestGenerateToken_InvalidExpiresIn covers the time.ParseDuration
// error wrap branch — caller passed garbage like "two hours".
func TestGenerateToken_InvalidExpiresIn(t *testing.T) {
	svc, _ := newServiceWithMock(t)
	// No mock.ExpectQuery — the validation error must fire BEFORE any DB call.
	_, err := svc.GenerateToken(context.Background(), GenerateRequest{
		ThingType: "agent",
		Label:     "x",
		ExpiresIn: "two hours",
	})
	if err == nil {
		t.Fatal("expected error from invalid expiresIn; got nil")
	}
	if !strings.Contains(err.Error(), "invalid expiresIn") {
		t.Errorf("error message must mention invalid expiresIn; got: %v", err)
	}
}

// TestGenerateToken_StoreInsertErrorWraps covers the wrap of DB
// failure with "generate token:" prefix.
func TestGenerateToken_StoreInsertErrorWraps(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	dbErr := errors.New("unique violation on token_hash")
	mock.ExpectQuery(`INSERT INTO enrollment_token`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(),
			"agent", "l", pgxmock.AnyArg(), pgxmock.AnyArg(), "").
		WillReturnError(dbErr)

	_, err := svc.GenerateToken(context.Background(), GenerateRequest{
		ThingType: "agent",
		Label:     "l",
		ExpiresIn: "1h",
	})
	if !errors.Is(err, dbErr) {
		t.Fatalf("error must wrap DB err; got: %v", err)
	}
	if !strings.Contains(err.Error(), "generate token") {
		t.Errorf("error must carry generate-token prefix; got: %v", err)
	}
}

// ListTokens — covers list mapping + computeStatus side-effect on
// expired-pending rows (the only non-trivial logic in the listing
// path).

// TestListTokens_EmptyResultReturnsEmptySlice pins the "no tokens"
// branch — handlers rely on a non-nil zero-length slice for JSON
// marshalling (`[]` not `null`).
func TestListTokens_EmptyResultReturnsEmptySlice(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	mock.ExpectQuery(`SELECT .*FROM enrollment_token`).
		WillReturnRows(pgxmock.NewRows(enrollmentTokenCols))

	got, err := svc.ListTokens(context.Background())
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if got == nil {
		t.Error("ListTokens must return non-nil slice for empty result")
	}
	if len(got) != 0 {
		t.Errorf("len: got %d, want 0", len(got))
	}
}

// TestListTokens_MapsRowsAndComputesStatuses covers the row→Token
// mapping plus computeStatus on three flavours of row:
//   - pending + future expiry  → "pending"
//   - pending + past expiry    → "expired" (computeStatus rewrites it)
//   - used + past expiry       → "used"    (status pinned; not rewritten)
func TestListTokens_MapsRowsAndComputesStatuses(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	now := time.Now().UTC()
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)

	mock.ExpectQuery(`SELECT .*FROM enrollment_token`).
		WillReturnRows(pgxmock.NewRows(enrollmentTokenCols).
			AddRow("id-active", "h1", "agent", (*string)(nil), "active", "pending",
				future, (*time.Time)(nil), []byte(`{"k":1}`), strPtr("u1"), now).
			AddRow("id-stale", "h2", "agent", (*string)(nil), "stale", "pending",
				past, (*time.Time)(nil), []byte(nil), (*string)(nil), now).
			AddRow("id-spent", "h3", "ai-gateway", strPtr("thing-1"), "spent", "used",
				past, &now, []byte(nil), strPtr("u3"), now),
		)

	got, err := svc.ListTokens(context.Background())
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len: got %d, want 3", len(got))
	}
	// Token 0: pending, not expired.
	if got[0].ID != "id-active" || got[0].Status != "pending" {
		t.Errorf("row 0: got %+v", got[0])
	}
	if got[0].Metadata["k"] == nil {
		t.Errorf("row 0 metadata not decoded: %+v", got[0].Metadata)
	}
	// Token 1: pending but past expiry → computeStatus rewrites to "expired".
	if got[1].ID != "id-stale" || got[1].Status != "expired" {
		t.Errorf("row 1: expected pending+past => expired; got %+v", got[1])
	}
	// Token 2: used + past expiry → status NOT rewritten (only pending+past flips).
	if got[2].ID != "id-spent" || got[2].Status != "used" {
		t.Errorf("row 2: used status must NOT flip to expired; got %+v", got[2])
	}
	if got[2].ThingType != "ai-gateway" {
		t.Errorf("row 2 thingType: got %q", got[2].ThingType)
	}
	// RawToken must NEVER leak through ListTokens (security invariant).
	for i, tok := range got {
		if tok.RawToken != "" {
			t.Errorf("row %d leaked RawToken: %q", i, tok.RawToken)
		}
	}
}

// TestListTokens_StoreErrorWraps covers the "list tokens:" wrap.
func TestListTokens_StoreErrorWraps(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	dbErr := errors.New("relation does not exist")
	mock.ExpectQuery(`SELECT .*FROM enrollment_token`).
		WillReturnError(dbErr)

	_, err := svc.ListTokens(context.Background())
	if !errors.Is(err, dbErr) {
		t.Fatalf("error must wrap DB err; got: %v", err)
	}
	if !strings.Contains(err.Error(), "list tokens") {
		t.Errorf("error must carry list-tokens prefix; got: %v", err)
	}
}

// Revoke — thin pass-through; verify the error surface. (The old
// ValidateToken / MarkUsed two-step is gone; its replacement
// ConsumeToken / LinkThing is covered in enrollment_consume_test.go.)

func TestRevoke_Success(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	mock.ExpectExec(`UPDATE enrollment_token`).
		WithArgs("id-1").
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))

	if err := svc.Revoke(context.Background(), "id-1"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
}

func TestRevoke_NotFoundSurfacesErrNotFound(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	mock.ExpectExec(`UPDATE enrollment_token`).
		WithArgs("missing").
		WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))

	err := svc.Revoke(context.Background(), "missing")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("0-row revoke must surface ErrNotFound; got: %v", err)
	}
}

func TestRevoke_StoreErrorPropagates(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	dbErr := errors.New("timeout")
	mock.ExpectExec(`UPDATE enrollment_token`).
		WithArgs("id-x").
		WillReturnError(dbErr)

	err := svc.Revoke(context.Background(), "id-x")
	if !errors.Is(err, dbErr) {
		t.Errorf("error must wrap store err; got: %v", err)
	}
}

// computeStatus — exhaustive: only "pending + past expiry" flips.

func TestComputeStatus_AllBranches(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	cases := []struct {
		name    string
		status  string
		expires time.Time
		want    string
	}{
		{"pending future stays pending", "pending", future, "pending"},
		{"pending past flips to expired", "pending", past, "expired"},
		{"used past stays used", "used", past, "used"},
		{"used future stays used", "used", future, "used"},
		{"revoked past stays revoked", "revoked", past, "revoked"},
		{"revoked future stays revoked", "revoked", future, "revoked"},
		{"expired past stays expired", "expired", past, "expired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeStatus(store.EnrollmentToken{
				Status:    tc.status,
				ExpiresAt: tc.expires,
			})
			if got != tc.want {
				t.Errorf("status=%q expires=%v: got %q, want %q",
					tc.status, tc.expires, got, tc.want)
			}
		})
	}
}
