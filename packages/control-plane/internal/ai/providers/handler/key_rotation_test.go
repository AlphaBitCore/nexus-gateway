// Coverage for key_rotation.go: RotateCredentialKey / GetKeyRotationStatus /
// rotateCredentialsWorker. The rotation worker is the credential-encryption
// migration path; failures here would block the operator's only knob for
// moving credentials between encryption-key versions.
package providers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// resetRotation clears the package-level rotationInProgress flag between
// tests so a prior test's "in flight" state never leaks.
func resetRotation(t *testing.T) {
	t.Helper()
	rotationInProgress.Store(false)
	t.Cleanup(func() { rotationInProgress.Store(false) })
}

func TestRotateCredentialKey_NoMultiVault400(t *testing.T) {
	resetRotation(t)
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.RotateCredentialKey(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "CREDENTIAL_KEY_MAP") {
		t.Errorf("body must surface CONFIGURATION_ERROR: %s", rec.Body.String())
	}
}

func TestRotateCredentialKey_AlreadyInProgress409(t *testing.T) {
	resetRotation(t)
	rotationInProgress.Store(true)
	mv := newTestMultiVault(t)
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, mv, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.RotateCredentialKey(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d; want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ROTATION_IN_PROGRESS") {
		t.Errorf("body must surface ROTATION_IN_PROGRESS: %s", rec.Body.String())
	}
}

func TestRotateCredentialKey_CountErrorResetsFlag(t *testing.T) {
	resetRotation(t)
	mock, db := newMockStore(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Credential" WHERE encryption_key_id`).
		WithArgs("v2").
		WillReturnError(errors.New("planner err"))
	mv := newTestMultiVault(t)
	h := newHandler(db, nil, &auditSpy{}, nil, nil, mv, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.RotateCredentialKey(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
	if rotationInProgress.Load() {
		t.Error("count failure must reset the in-progress flag")
	}
}

func TestRotateCredentialKey_HappyKicksOffWorker(t *testing.T) {
	resetRotation(t)
	mock, db := newMockStore(t)
	// initial count query — 0 pending so the worker exits cleanly.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Credential" WHERE encryption_key_id`).
		WithArgs("v2").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(3))
	// worker's first ListCredentialsForRotation returns no rows so it exits
	// fast — the goroutine + DB expectations + invalidation are exercised.
	mock.ExpectQuery(`FROM "Credential"\s+WHERE encryption_key_id != \$1\s+ORDER BY "createdAt"`).
		WithArgs("v2", 50).
		WillReturnRows(pgxmock.NewRows(credentialEncryptedCols))
	hub := &hubSpy{}
	aud := &auditSpy{}
	mv := newTestMultiVault(t)
	h := newHandler(db, hub, aud, nil, nil, mv, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.RotateCredentialKey(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["status"] != "rotating" || resp["targetKeyId"] != "v2" || resp["pendingCount"].(float64) != 3 {
		t.Errorf("resp = %+v", resp)
	}
	if aud.count() != 1 {
		t.Errorf("audit count = %d; want 1", aud.count())
	}
	// Wait for the worker goroutine to drain (no pending rows → returns
	// immediately, fires the hub invalidate, then resets the flag).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && rotationInProgress.Load() {
		time.Sleep(5 * time.Millisecond)
	}
	if rotationInProgress.Load() {
		t.Error("worker should have cleared the in-progress flag")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
	seen := hub.seen()
	found := false
	for _, s := range seen {
		if s == "ai-gateway/credentials" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected ai-gateway/credentials invalidate; got %v", seen)
	}
}

func TestGetKeyRotationStatus_NoMultiVault(t *testing.T) {
	resetRotation(t)
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.GetKeyRotationStatus(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"not_configured"`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestGetKeyRotationStatus_IdleAndRotating(t *testing.T) {
	resetRotation(t)
	// Two count queries — one per status read.
	for _, want := range []string{"idle", "rotating"} {
		mock, db := newMockStore(t)
		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Credential" WHERE encryption_key_id`).
			WithArgs("v2").
			WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(5))
		mv := newTestMultiVault(t)
		h := newHandler(db, nil, &auditSpy{}, nil, nil, mv, ProxyConfig{})
		rotationInProgress.Store(want == "rotating")

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		c, _ := echoCtx(req, rec, "u-1")
		if err := h.GetKeyRotationStatus(c); err != nil {
			t.Fatalf("err: %v", err)
		}
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp["status"] != want || resp["targetKeyId"] != "v2" || resp["pendingCount"].(float64) != 5 {
			t.Errorf("resp = %+v; want status=%s", resp, want)
		}
		rotationInProgress.Store(false)
	}
}

func TestGetKeyRotationStatus_CountErrorYieldsZero(t *testing.T) {
	resetRotation(t)
	mock, db := newMockStore(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Credential" WHERE encryption_key_id`).
		WithArgs("v2").
		WillReturnError(errors.New("planner err"))
	mv := newTestMultiVault(t)
	h := newHandler(db, nil, &auditSpy{}, nil, nil, mv, ProxyConfig{})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.GetKeyRotationStatus(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	// Count error swallowed — handler still returns 200 with pendingCount=0.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["pendingCount"].(float64) != 0 {
		t.Errorf("pendingCount = %v; want 0", resp["pendingCount"])
	}
}

// rotateCredentialsWorker — direct invocation

func TestRotateCredentialsWorker_ListErrorExits(t *testing.T) {
	resetRotation(t)
	rotationInProgress.Store(true)
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "Credential"`).
		WithArgs("v2", 50).
		WillReturnError(errors.New("planner err"))
	mv := newTestMultiVault(t)
	hub := &hubSpy{}
	h := newHandler(db, hub, &auditSpy{}, nil, nil, mv, ProxyConfig{})
	h.rotateCredentialsWorker("v2")

	if rotationInProgress.Load() {
		t.Error("worker must reset flag even on list error")
	}
	// On list error the worker returns early — no hub invalidate fires.
	if len(hub.seen()) != 0 {
		t.Errorf("worker must not invalidate when list errors; got %v", hub.seen())
	}
}

func TestRotateCredentialsWorker_DecryptEncryptUpdateAllPaths(t *testing.T) {
	resetRotation(t)
	rotationInProgress.Store(true)
	mock, db := newMockStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	mv := newTestMultiVault(t)

	// Build a real ciphertext encrypted under v1 so multiVault.Decrypt
	// succeeds for the rotation roundtrip on row 1. The MultiVault always
	// encrypts with the current key (v2), so we drive v1 directly via a
	// standalone Vault keyed off the same raw bytes the MultiVault parses.
	v1Vault := vaultForKey(t, "v1")
	row1Enc, err := v1Vault.Encrypt("plaintext-row-1")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// row 2 has a bogus key id so Decrypt fails — exercises the
	// per-row continue path.
	row2 := makeCredentialEncryptedRow(now, "unknown-key")
	row2[0] = "cred-2"
	// Row 1: enc fields = real ciphertext under v1.
	row1 := makeCredentialEncryptedRow(now, "v1")
	row1[0] = "cred-1"
	// Replace EncryptedKey/EncryptionIV/EncryptionTag (positions 30/31/32 — 0-indexed = 30,31,32).
	row1[len(credentialMetadataCols)+0] = row1Enc.Ciphertext
	row1[len(credentialMetadataCols)+1] = row1Enc.IV
	row1[len(credentialMetadataCols)+2] = row1Enc.Tag

	// First batch: 2 rows.
	mock.ExpectQuery(`FROM "Credential"`).
		WithArgs("v2", 50).
		WillReturnRows(pgxmock.NewRows(credentialEncryptedCols).
			AddRow(row1...).AddRow(row2...))
	// row1 update succeeds.
	mock.ExpectExec(`UPDATE "Credential"\s+SET "encryptedKey"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), "v2").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	// Second batch: empty → loop exits.
	mock.ExpectQuery(`FROM "Credential"`).
		WithArgs("v2", 50).
		WillReturnRows(pgxmock.NewRows(credentialEncryptedCols))

	hub := &hubSpy{}
	h := newHandler(db, hub, &auditSpy{}, nil, nil, mv, ProxyConfig{})
	h.rotateCredentialsWorker("v2")

	if rotationInProgress.Load() {
		t.Error("worker must clear flag on success")
	}
	seen := hub.seen()
	found := false
	for _, s := range seen {
		if s == "ai-gateway/credentials" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected ai-gateway/credentials invalidate; got %v", seen)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestRotateCredentialsWorker_UpdateFailureContinues(t *testing.T) {
	// When a re-encrypt update fails for a row, the worker logs and
	// continues — must not abort the batch. We arrange a single row whose
	// update returns an error; the next batch query is then empty so the
	// loop exits.
	resetRotation(t)
	rotationInProgress.Store(true)
	mock, db := newMockStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	mv := newTestMultiVault(t)
	v1Vault := vaultForKey(t, "v1")
	rowEnc, err := v1Vault.Encrypt("plaintext")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	row := makeCredentialEncryptedRow(now, "v1")
	row[len(credentialMetadataCols)+0] = rowEnc.Ciphertext
	row[len(credentialMetadataCols)+1] = rowEnc.IV
	row[len(credentialMetadataCols)+2] = rowEnc.Tag
	mock.ExpectQuery(`FROM "Credential"`).
		WithArgs("v2", 50).
		WillReturnRows(pgxmock.NewRows(credentialEncryptedCols).AddRow(row...))
	mock.ExpectExec(`UPDATE "Credential"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), "v2").
		WillReturnError(errors.New("update failed"))
	mock.ExpectQuery(`FROM "Credential"`).
		WithArgs("v2", 50).
		WillReturnRows(pgxmock.NewRows(credentialEncryptedCols))

	hub := &hubSpy{}
	h := newHandler(db, hub, &auditSpy{}, nil, nil, mv, ProxyConfig{})
	h.rotateCredentialsWorker("v2")
	if rotationInProgress.Load() {
		t.Error("worker must clear flag even when row updates fail")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// silence the io import warning if other test stages drift.
var _ = io.Discard
