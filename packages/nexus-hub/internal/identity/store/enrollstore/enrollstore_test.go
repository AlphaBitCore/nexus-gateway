// Tests for the enrollstore package.
package enrollstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

// decodeJSONB helper

func TestDecodeJSONB_EmptyInput(t *testing.T) {
	var out map[string]any
	if err := decodeJSONB(nil, &out, "col"); err != nil {
		t.Errorf("empty input should return nil, got %v", err)
	}
}

func TestDecodeJSONB_ValidJSON(t *testing.T) {
	var out map[string]any
	if err := decodeJSONB([]byte(`{"k":"v"}`), &out, "col"); err != nil {
		t.Errorf("valid JSON: %v", err)
	}
	if out["k"] != "v" {
		t.Errorf("decoded value = %v, want v", out["k"])
	}
}

func TestDecodeJSONB_InvalidJSON(t *testing.T) {
	var out map[string]any
	if err := decodeJSONB([]byte(`bad json`), &out, "col"); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestHashTokenSHA256_Deterministic(t *testing.T) {
	h1 := hashTokenSHA256("test-token")
	h2 := hashTokenSHA256("test-token")
	if h1 != h2 {
		t.Errorf("hash is not deterministic: %s vs %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("hash length = %d, want 64 (SHA-256 hex)", len(h1))
	}
}

func TestHashTokenSHA256_Distinct(t *testing.T) {
	if hashTokenSHA256("a") == hashTokenSHA256("b") {
		t.Error("different inputs should produce different hashes")
	}
}

func TestGetThingAgentForTrustLevel_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	expiry := time.Now().Add(24 * time.Hour)
	mock.ExpectQuery(`FROM thing_agent`).
		WithArgs("thing-1").
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "version", "cert_expires_at"}).
			AddRow("thing-1", "1.2.3", &expiry))

	s := New(mock)
	rec, err := s.GetThingAgentForTrustLevel(context.Background(), "thing-1")
	if err != nil {
		t.Fatalf("GetThingAgentForTrustLevel: %v", err)
	}
	if rec.ThingID != "thing-1" || rec.Version != "1.2.3" {
		t.Errorf("got %+v, want {thing-1, 1.2.3}", rec)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestGetThingAgentForTrustLevel_NotFound(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM thing_agent`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	s := New(mock)
	_, err := s.GetThingAgentForTrustLevel(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestGetThingAgentForTrustLevel_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("db error")
	mock.ExpectQuery(`FROM thing_agent`).
		WithArgs("t1").
		WillReturnError(sentinel)

	s := New(mock)
	_, err := s.GetThingAgentForTrustLevel(context.Background(), "t1")
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel", err)
	}
}

func TestHasActiveDeviceAssignment_True(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs("device-1").
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))

	s := New(mock)
	has, err := s.HasActiveDeviceAssignment(context.Background(), "device-1")
	if err != nil {
		t.Fatalf("HasActiveDeviceAssignment: %v", err)
	}
	if !has {
		t.Error("expected has=true")
	}
}

func TestHasActiveDeviceAssignment_False(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs("device-2").
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(false))

	s := New(mock)
	has, err := s.HasActiveDeviceAssignment(context.Background(), "device-2")
	if err != nil {
		t.Fatalf("HasActiveDeviceAssignment: %v", err)
	}
	if has {
		t.Error("expected has=false")
	}
}

func TestHasActiveDeviceAssignment_Error(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("db error")
	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs("d1").
		WillReturnError(sentinel)

	s := New(mock)
	_, err := s.HasActiveDeviceAssignment(context.Background(), "d1")
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel", err)
	}
}

func TestUpdateThingAgentTrustLevel_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectExec(`UPDATE thing_agent`).
		WithArgs("thing-1", 3).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	s := New(mock)
	if err := s.UpdateThingAgentTrustLevel(context.Background(), "thing-1", 3); err != nil {
		t.Fatalf("UpdateThingAgentTrustLevel: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestUpdateThingAgentTrustLevel_NotFound(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectExec(`UPDATE thing_agent`).
		WithArgs("missing", 2).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	s := New(mock)
	err := s.UpdateThingAgentTrustLevel(context.Background(), "missing", 2)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestUpdateThingAgentTrustLevel_ExecError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("db error")
	mock.ExpectExec(`UPDATE thing_agent`).
		WithArgs("t1", 1).
		WillReturnError(sentinel)

	s := New(mock)
	err := s.UpdateThingAgentTrustLevel(context.Background(), "t1", 1)
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel", err)
	}
}

func TestUpsertDeviceAssignment_EmptyThingID(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// No expectations — early return for empty ThingID.
	s := New(mock)
	if err := s.UpsertDeviceAssignment(context.Background(), UpsertDeviceAssignmentParams{
		ThingID: "", UserID: "u1",
	}); err != nil {
		t.Errorf("empty ThingID should return nil, got %v", err)
	}
}

func TestUpsertDeviceAssignment_EmptyUserID(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// No expectations — early return for empty UserID.
	s := New(mock)
	if err := s.UpsertDeviceAssignment(context.Background(), UpsertDeviceAssignmentParams{
		ThingID: "t1", UserID: "",
	}); err != nil {
		t.Errorf("empty UserID should return nil, got %v", err)
	}
}

func TestUpsertDeviceAssignment_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Step 1: release stale assignment.
	mock.ExpectExec(`UPDATE "DeviceAssignment"`).
		WithArgs("t1", "u1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	// Step 2: insert new assignment.
	mock.ExpectExec(`INSERT INTO "DeviceAssignment"`).
		WithArgs("t1", "u1", string(DeviceAssignmentSourceSSO), "", "").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// Step 3: sync thing_agent.current_assignment_id.
	mock.ExpectExec(`UPDATE thing_agent`).
		WithArgs("t1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	s := New(mock)
	err := s.UpsertDeviceAssignment(context.Background(), UpsertDeviceAssignmentParams{
		ThingID: "t1",
		UserID:  "u1",
		Source:  DeviceAssignmentSourceSSO,
	})
	if err != nil {
		t.Fatalf("UpsertDeviceAssignment: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestUpsertDeviceAssignment_Step1Error(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("step1 err")
	mock.ExpectExec(`UPDATE "DeviceAssignment"`).
		WithArgs("t1", "u1").
		WillReturnError(sentinel)

	s := New(mock)
	err := s.UpsertDeviceAssignment(context.Background(), UpsertDeviceAssignmentParams{
		ThingID: "t1", UserID: "u1", Source: DeviceAssignmentSourceAdmin,
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel", err)
	}
}

func TestUpsertDeviceAssignment_Step2Error(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("step2 err")
	mock.ExpectExec(`UPDATE "DeviceAssignment"`).
		WithArgs("t1", "u1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec(`INSERT INTO "DeviceAssignment"`).
		WithArgs("t1", "u1", string(DeviceAssignmentSourceAdmin), "", "").
		WillReturnError(sentinel)

	s := New(mock)
	err := s.UpsertDeviceAssignment(context.Background(), UpsertDeviceAssignmentParams{
		ThingID: "t1", UserID: "u1", Source: DeviceAssignmentSourceAdmin,
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel", err)
	}
}

// TestUpsertDeviceAssignment_Step3ErrorWarnOnly verifies that Step 3
// (thing_agent sync) failure does NOT propagate to the caller — it's
// warn+continue.
func TestUpsertDeviceAssignment_Step3ErrorWarnOnly(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectExec(`UPDATE "DeviceAssignment"`).
		WithArgs("t1", "u1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec(`INSERT INTO "DeviceAssignment"`).
		WithArgs("t1", "u1", string(DeviceAssignmentSourceSSO), "sso", "1.2.3.4").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// Step 3 fails — should be a warn, not a returned error.
	mock.ExpectExec(`UPDATE thing_agent`).
		WithArgs("t1").
		WillReturnError(errors.New("step3 err"))

	s := New(mock)
	err := s.UpsertDeviceAssignment(context.Background(), UpsertDeviceAssignmentParams{
		ThingID:     "t1",
		UserID:      "u1",
		Source:      DeviceAssignmentSourceSSO,
		LoginMethod: "sso",
		IPAddress:   "1.2.3.4",
	})
	if err != nil {
		t.Errorf("step3 error should not propagate, got %v", err)
	}
}

func TestRefreshActiveDeviceAssignmentIP_EmptyThingID(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	s := New(mock)
	changed, err := s.RefreshActiveDeviceAssignmentIP(context.Background(), "", "1.2.3.4")
	if err != nil || changed {
		t.Errorf("empty thingID: err=%v changed=%v, want nil false", err, changed)
	}
}

func TestRefreshActiveDeviceAssignmentIP_EmptyIP(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	s := New(mock)
	changed, err := s.RefreshActiveDeviceAssignmentIP(context.Background(), "t1", "")
	if err != nil || changed {
		t.Errorf("empty IP: err=%v changed=%v, want nil false", err, changed)
	}
}

func TestRefreshActiveDeviceAssignmentIP_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectExec(`UPDATE "DeviceAssignment"`).
		WithArgs("t1", "5.6.7.8").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	s := New(mock)
	changed, err := s.RefreshActiveDeviceAssignmentIP(context.Background(), "t1", "5.6.7.8")
	if err != nil {
		t.Fatalf("RefreshActiveDeviceAssignmentIP: %v", err)
	}
	if !changed {
		t.Error("expected changed=true")
	}
}

func TestRefreshActiveDeviceAssignmentIP_NoChange(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectExec(`UPDATE "DeviceAssignment"`).
		WithArgs("t1", "1.2.3.4").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	s := New(mock)
	changed, err := s.RefreshActiveDeviceAssignmentIP(context.Background(), "t1", "1.2.3.4")
	if err != nil || changed {
		t.Errorf("no-change case: err=%v changed=%v, want nil false", err, changed)
	}
}

func TestRefreshActiveDeviceAssignmentIP_ExecError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("exec err")
	mock.ExpectExec(`UPDATE "DeviceAssignment"`).
		WithArgs("t1", "1.1.1.1").
		WillReturnError(sentinel)

	s := New(mock)
	_, err := s.RefreshActiveDeviceAssignmentIP(context.Background(), "t1", "1.1.1.1")
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel", err)
	}
}

// The ValidateEnrollmentToken / MarkEnrollmentTokenUsed two-step was removed
// in F-0204 (replaced by the atomic ConsumeEnrollmentToken +
// LinkEnrollmentTokenThing pair, covered in enrollment_token_consume_test.go).

func TestRevokeEnrollmentToken_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectExec(`UPDATE enrollment_token SET status = 'revoked'`).
		WithArgs("tok-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	s := New(mock)
	if err := s.RevokeEnrollmentToken(context.Background(), "tok-1"); err != nil {
		t.Fatalf("RevokeEnrollmentToken: %v", err)
	}
}

func TestRevokeEnrollmentToken_NotFound(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectExec(`UPDATE enrollment_token SET status = 'revoked'`).
		WithArgs("tok-2").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	s := New(mock)
	err := s.RevokeEnrollmentToken(context.Background(), "tok-2")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestRevokeEnrollmentToken_ExecError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("exec err")
	mock.ExpectExec(`UPDATE enrollment_token SET status = 'revoked'`).
		WithArgs("tok-3").
		WillReturnError(sentinel)

	s := New(mock)
	err := s.RevokeEnrollmentToken(context.Background(), "tok-3")
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel", err)
	}
}

func TestListEnrollmentTokens_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	now := time.Now()
	cols := []string{"id", "token_hash", "thing_type", "thing_id", "label", "status", "expires_at", "used_at", "metadata", "created_by", "created_at"}
	mock.ExpectQuery(`FROM enrollment_token`).
		WillReturnRows(pgxmock.NewRows(cols).
			AddRow("t1", "hash1", "agent", nil, "lab1", "pending", now, nil, []byte(`{}`), nil, now))

	s := New(mock)
	tokens, err := s.ListEnrollmentTokens(context.Background(), "", "")
	if err != nil {
		t.Fatalf("ListEnrollmentTokens: %v", err)
	}
	if len(tokens) != 1 || tokens[0].ID != "t1" {
		t.Errorf("got %+v, want [{id:t1}]", tokens)
	}
}

func TestListEnrollmentTokens_WithFilters(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	now := time.Now()
	cols := []string{"id", "token_hash", "thing_type", "thing_id", "label", "status", "expires_at", "used_at", "metadata", "created_by", "created_at"}
	mock.ExpectQuery(`FROM enrollment_token`).
		WithArgs("agent", "pending").
		WillReturnRows(pgxmock.NewRows(cols).
			AddRow("t2", "hash2", "agent", nil, "lab2", "pending", now, nil, nil, nil, now))

	s := New(mock)
	tokens, err := s.ListEnrollmentTokens(context.Background(), "agent", "pending")
	if err != nil {
		t.Fatalf("ListEnrollmentTokens with filters: %v", err)
	}
	if len(tokens) != 1 {
		t.Errorf("expected 1 token, got %d", len(tokens))
	}
}

func TestListEnrollmentTokens_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("query err")
	mock.ExpectQuery(`FROM enrollment_token`).
		WillReturnError(sentinel)

	s := New(mock)
	_, err := s.ListEnrollmentTokens(context.Background(), "", "")
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel", err)
	}
}

func TestListEnrollmentTokens_RowsErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("rows err")
	cols := []string{"id", "token_hash", "thing_type", "thing_id", "label", "status", "expires_at", "used_at", "metadata", "created_by", "created_at"}
	mock.ExpectQuery(`FROM enrollment_token`).
		WillReturnRows(pgxmock.NewRows(cols).CloseError(sentinel))

	s := New(mock)
	_, err := s.ListEnrollmentTokens(context.Background(), "", "")
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel", err)
	}
}

func TestCleanupExpiredEnrollmentTokens_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectExec(`UPDATE enrollment_token SET status = 'expired'`).
		WillReturnResult(pgxmock.NewResult("UPDATE", 5))

	s := New(mock)
	count, err := s.CleanupExpiredEnrollmentTokens(context.Background())
	if err != nil {
		t.Fatalf("CleanupExpiredEnrollmentTokens: %v", err)
	}
	if count != 5 {
		t.Errorf("count = %d, want 5", count)
	}
}

func TestCleanupExpiredEnrollmentTokens_ExecError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("exec err")
	mock.ExpectExec(`UPDATE enrollment_token SET status = 'expired'`).
		WillReturnError(sentinel)

	s := New(mock)
	_, err := s.CleanupExpiredEnrollmentTokens(context.Background())
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel", err)
	}
}

// InsertEnrollmentToken — exercises the happy path including rand.Read.

func TestInsertEnrollmentToken_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	now := time.Now()
	cols := []string{"id", "token_hash", "thing_type", "thing_id", "label", "status", "expires_at", "used_at", "metadata", "created_by", "created_at"}
	// 7 args: id, tokenHash, ThingType, Label, expiresAt, metaJSON (nil), CreatedBy.
	mock.ExpectQuery(`INSERT INTO enrollment_token`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "agent", "my-label", pgxmock.AnyArg(), pgxmock.AnyArg(), "admin").
		WillReturnRows(pgxmock.NewRows(cols).
			AddRow("gen-id", "hash", "agent", nil, "my-label", "pending", now.Add(24*time.Hour), nil, nil, nil, now))

	s := New(mock)
	et, rawToken, err := s.InsertEnrollmentToken(context.Background(), InsertEnrollmentTokenParams{
		ThingType: "agent",
		Label:     "my-label",
		ExpiresIn: 24 * time.Hour,
		CreatedBy: "admin",
	})
	if err != nil {
		t.Fatalf("InsertEnrollmentToken: %v", err)
	}
	if et == nil {
		t.Fatal("expected non-nil token record")
	}
	if rawToken == "" {
		t.Error("expected non-empty raw token")
	}
}

func TestInsertEnrollmentToken_DefaultExpiresIn(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	now := time.Now()
	cols := []string{"id", "token_hash", "thing_type", "thing_id", "label", "status", "expires_at", "used_at", "metadata", "created_by", "created_at"}
	// ExpiresIn <= 0 → defaults to 24h. metaJSON=nil (no metadata).
	mock.ExpectQuery(`INSERT INTO enrollment_token`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "agent", "label", pgxmock.AnyArg(), pgxmock.AnyArg(), "").
		WillReturnRows(pgxmock.NewRows(cols).
			AddRow("id2", "hash2", "agent", nil, "label", "pending", now.Add(24*time.Hour), nil, nil, nil, now))

	s := New(mock)
	_, _, err := s.InsertEnrollmentToken(context.Background(), InsertEnrollmentTokenParams{
		ThingType: "agent",
		Label:     "label",
		ExpiresIn: 0, // should default to 24h
	})
	if err != nil {
		t.Fatalf("InsertEnrollmentToken default ExpiresIn: %v", err)
	}
}

func TestInsertEnrollmentToken_WithMetadata(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	now := time.Now()
	cols := []string{"id", "token_hash", "thing_type", "thing_id", "label", "status", "expires_at", "used_at", "metadata", "created_by", "created_at"}
	mock.ExpectQuery(`INSERT INTO enrollment_token`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "agent", "lab", pgxmock.AnyArg(), pgxmock.AnyArg(), "").
		WillReturnRows(pgxmock.NewRows(cols).
			AddRow("id3", "hash3", "agent", nil, "lab", "pending", now.Add(time.Hour), nil, []byte(`{"k":"v"}`), nil, now))

	s := New(mock)
	et, _, err := s.InsertEnrollmentToken(context.Background(), InsertEnrollmentTokenParams{
		ThingType: "agent",
		Label:     "lab",
		ExpiresIn: time.Hour,
		Metadata:  map[string]any{"k": "v"},
	})
	if err != nil {
		t.Fatalf("InsertEnrollmentToken with metadata: %v", err)
	}
	if et.Metadata["k"] != "v" {
		t.Errorf("metadata k = %v, want v", et.Metadata["k"])
	}
}

func TestInsertEnrollmentToken_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("insert err")
	mock.ExpectQuery(`INSERT INTO enrollment_token`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "agent", "label", pgxmock.AnyArg(), pgxmock.AnyArg(), "").
		WillReturnError(sentinel)

	s := New(mock)
	_, _, err := s.InsertEnrollmentToken(context.Background(), InsertEnrollmentTokenParams{
		ThingType: "agent",
		Label:     "label",
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel", err)
	}
}
