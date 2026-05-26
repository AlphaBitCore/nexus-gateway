package iam

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
)

// rotation handler tests live in their own file because the existing
// users_handler_test.go is already ~5000 LOC; per-feature test files keep the
// per-handler arrangement scannable.

func TestRotateAPIKey_NotFound_Returns404(t *testing.T) {
	us := &stubUserStore{rotateErr: pgx.ErrNoRows}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys/missing/rotate", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.RotateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404; body=%s", rec.Code, rec.Body)
	}
}

func TestRotateAPIKey_PredecessorNotActive_Returns409(t *testing.T) {
	us := &stubUserStore{
		rotateErr: errors.New("rotate: predecessor status is \"rotating\"; only active keys can be rotated"),
	}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys/k1/rotate", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.RotateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("code=%d want 409 (predecessor not active); body=%s", rec.Code, rec.Body)
	}
	// Body must surface the validation error code so the UI can show the
	// specific reason rather than a generic "rotate failed" string.
	var payload map[string]any
	if e := json.Unmarshal(rec.Body.Bytes(), &payload); e != nil {
		t.Fatalf("decode body: %v", e)
	}
	errObj, _ := payload["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "ROTATE_INVALID_STATE" {
		t.Errorf("error.code=%q want ROTATE_INVALID_STATE", code)
	}
}

func TestRotateAPIKey_DBError_Returns500(t *testing.T) {
	us := &stubUserStore{rotateErr: errors.New("rotate: insert successor: connection reset")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys/k1/rotate", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.RotateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500; body=%s", rec.Code, rec.Body)
	}
}

func TestRotateAPIKey_Success_Returns201WithKeyAndPredecessorRotating(t *testing.T) {
	uid := "u1"
	rotatedAt := time.Now().UTC()
	us := &stubUserStore{
		rotateResult: &userstore.RotateAdminAPIKeyResult{
			Successor: &userstore.AdminAPIKey{
				ID: "k2", Name: "Key", KeyPrefix: "nxk_aaaabbbb",
				Status: userstore.AdminAPIKeyStatusActive, OwnerUserID: &uid,
			},
			Predecessor: &userstore.AdminAPIKey{
				ID: "k1", Name: "Key", KeyPrefix: "nxk_oldoldol",
				Status: userstore.AdminAPIKeyStatusRotating, OwnerUserID: &uid,
				RotatedAt: &rotatedAt,
			},
		},
	}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys/k1/rotate", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.RotateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
	// Verify the response carries the new plaintext key (visible-once
	// contract) AND the predecessor's new rotating status — both signals
	// the UI needs to render the in-flight rotation chain.
	var payload map[string]any
	if e := json.Unmarshal(rec.Body.Bytes(), &payload); e != nil {
		t.Fatalf("decode body: %v", e)
	}
	rawKey, _ := payload["key"].(string)
	if !strings.HasPrefix(rawKey, "nxk_") {
		t.Errorf("response.key=%q must begin with the nxk_ prefix (visible-once plaintext)", rawKey)
	}
	pred, _ := payload["predecessor"].(map[string]any)
	if pred == nil {
		t.Fatal("response missing predecessor object")
	}
	if status, _ := pred["status"].(string); status != userstore.AdminAPIKeyStatusRotating {
		t.Errorf("predecessor.status=%q want %q", status, userstore.AdminAPIKeyStatusRotating)
	}
	if id, _ := pred["id"].(string); id != "k1" {
		t.Errorf("predecessor.id=%q want k1", id)
	}
}

func TestRotateAPIKey_InvalidExpiresAt_Returns400(t *testing.T) {
	us := &stubUserStore{}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body := []byte(`{"expiresAt":"not-a-date"}`)
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys/k1/rotate", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.RotateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400; body=%s", rec.Code, rec.Body)
	}
}

func TestRetireAPIKey_NotFound_Returns404(t *testing.T) {
	us := &stubUserStore{retireErr: pgx.ErrNoRows}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodPut, "/api-keys/missing/retire", []byte(`{"targetStatus":"expired"}`), "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.RetireAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404; body=%s", rec.Code, rec.Body)
	}
}

func TestRetireAPIKey_AlreadyTerminal_Returns409(t *testing.T) {
	us := &stubUserStore{retireErr: errors.New("retire: key is already expired or unavailable")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodPut, "/api-keys/k1/retire", []byte(`{"targetStatus":"expired"}`), "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.RetireAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("code=%d want 409 (already terminal); body=%s", rec.Code, rec.Body)
	}
}

func TestRetireAPIKey_InvalidTargetStatus_Returns400(t *testing.T) {
	us := &stubUserStore{}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodPut, "/api-keys/k1/retire", []byte(`{"targetStatus":"bogus"}`), "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.RetireAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400; body=%s", rec.Code, rec.Body)
	}
}

func TestRetireAPIKey_SelfKey_Returns409(t *testing.T) {
	us := &stubUserStore{}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodPut, "/api-keys/k1/retire", []byte(`{"targetStatus":"expired"}`), "k1", "api_key")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.RetireAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("code=%d want 409 (self-key in use); body=%s", rec.Code, rec.Body)
	}
}

func TestRetireAPIKey_Expired_Success_Returns200(t *testing.T) {
	us := &stubUserStore{
		getKey:    &userstore.AdminAPIKey{ID: "k1", Status: userstore.AdminAPIKeyStatusRotating},
		retireKey: &userstore.AdminAPIKey{ID: "k1", Status: userstore.AdminAPIKeyStatusExpired},
	}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodPut, "/api-keys/k1/retire", []byte(`{"targetStatus":"expired"}`), "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.RetireAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
	var payload map[string]any
	if e := json.Unmarshal(rec.Body.Bytes(), &payload); e != nil {
		t.Fatalf("decode body: %v", e)
	}
	data, _ := payload["data"].(map[string]any)
	if status, _ := data["status"].(string); status != userstore.AdminAPIKeyStatusExpired {
		t.Errorf("data.status=%q want %q", status, userstore.AdminAPIKeyStatusExpired)
	}
}

func TestRetireAPIKey_Unavailable_Success_Returns200(t *testing.T) {
	us := &stubUserStore{
		getKey:    &userstore.AdminAPIKey{ID: "k1", Status: userstore.AdminAPIKeyStatusActive},
		retireKey: &userstore.AdminAPIKey{ID: "k1", Status: userstore.AdminAPIKeyStatusUnavailable},
	}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodPut, "/api-keys/k1/retire", []byte(`{"targetStatus":"unavailable"}`), "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.RetireAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
	var payload map[string]any
	if e := json.Unmarshal(rec.Body.Bytes(), &payload); e != nil {
		t.Fatalf("decode body: %v", e)
	}
	data, _ := payload["data"].(map[string]any)
	if status, _ := data["status"].(string); status != userstore.AdminAPIKeyStatusUnavailable {
		t.Errorf("data.status=%q want %q", status, userstore.AdminAPIKeyStatusUnavailable)
	}
}

func TestRetireAPIKey_EmptyBody_DefaultsToExpired(t *testing.T) {
	us := &stubUserStore{
		getKey:    &userstore.AdminAPIKey{ID: "k1", Status: userstore.AdminAPIKeyStatusActive},
		retireKey: &userstore.AdminAPIKey{ID: "k1", Status: userstore.AdminAPIKeyStatusExpired},
	}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	// Empty body — server must default targetStatus to "expired" so the
	// most-common operator flow ("retire this key") is one click.
	c, rec := adminAuthCtx(http.MethodPut, "/api-keys/k1/retire", []byte(`{}`), "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.RetireAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// containsStatusInvariantHint / containsAlreadyRetiredHint are tiny helpers
// that downgrade store errors to HTTP 409. Cover them with explicit table
// tests so a future store-helper message change does not silently regress
// 409 → 500 (which would surface to operators as a generic outage).

func TestContainsStatusInvariantHint(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		{"empty", "", false},
		{"unrelated", "connection reset by peer", false},
		{"matching", "rotate: predecessor status is \"rotating\"; only active keys can be rotated", true},
		{"substring only", "only active keys can be rotated", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := containsStatusInvariantHint(tc.msg); got != tc.want {
				t.Errorf("containsStatusInvariantHint(%q)=%v want %v", tc.msg, got, tc.want)
			}
		})
	}
}

func TestContainsAlreadyRetiredHint(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		{"empty", "", false},
		{"unrelated", "deadline exceeded", false},
		{"matching", "retire: key is already expired or unavailable", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := containsAlreadyRetiredHint(tc.msg); got != tc.want {
				t.Errorf("containsAlreadyRetiredHint(%q)=%v want %v", tc.msg, got, tc.want)
			}
		})
	}
}
