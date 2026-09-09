package iam

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
)

// An admin API key authenticates AS its owner, so `expiresAt` is the only thing
// bounding its lifetime. Parsing it in create and update with
// `if err == nil { expiresAt = &t }` drops a malformed value and lets the
// endpoint carry on, minting a PERMANENT credential and answering 201 — while Rotate,
// in the same file, refuses. These arms pin the whole surface to one answer.

func keyCreateHandler(us *stubUserStore) *Handler {
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.iamEngine = superAdminTestEngine()
	return h
}

func createStub() *stubUserStore {
	uid := "u1"
	return &stubUserStore{createKey: &userstore.AdminAPIKey{ID: "k1", Name: "Key", OwnerUserID: &uid}}
}

// unreadableExpiries are values a caller plausibly sends. Every one of them
// minted a never-expiring admin key before the fix: a JS `Date.toString()`, a
// bare date, an epoch, a nonsense month, and a value that is RFC3339-shaped but
// not RFC3339.
var unreadableExpiries = []string{
	"Mon Jan 01 2029 00:00:00 GMT+0000",
	"2029-01-01",
	"1861920000",
	"2029-13-45T00:00:00Z",
	"2029-01-01 00:00:00",
	"tomorrow",
}

func TestCreateAPIKey_UnreadableExpiry_RefusesAndMintsNothing(t *testing.T) {
	for _, raw := range unreadableExpiries {
		t.Run(raw, func(t *testing.T) {
			us := createStub()
			h := keyCreateHandler(us)
			body, _ := json.Marshal(map[string]any{"name": "Key", "expiresAt": raw})
			c, rec := adminAuthCtx(http.MethodPost, "/api-keys", body, "admin", "admin_user")
			if err := h.CreateAPIKey(c); err != nil {
				t.Fatal(err)
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("code=%d want 400 — an expiry we cannot read must not mint a key; body=%s", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), "INVALID_EXPIRES_AT") {
				t.Errorf("body=%s want INVALID_EXPIRES_AT", rec.Body)
			}
			// The refusal has to land BEFORE key material exists. A 400 that
			// still wrote a row would leave a credential nobody was told about.
			if us.createKeyCalled {
				t.Error("the store was asked to persist a key despite the refusal")
			}
		})
	}
}

func TestCreateAPIKey_PastExpiry_Refused(t *testing.T) {
	us := createStub()
	h := keyCreateHandler(us)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	body, _ := json.Marshal(map[string]any{"name": "Key", "expiresAt": past})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys", body, "admin", "admin_user")
	if err := h.CreateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "EXPIRES_AT_IN_PAST") {
		t.Errorf("code=%d body=%s want 400 EXPIRES_AT_IN_PAST — every other expiry-bearing grant in this service refuses one", rec.Code, rec.Body)
	}
	if us.createKeyCalled {
		t.Error("an expired-on-arrival key was persisted")
	}
}

// The sibling arm: a value we CAN read must reach the store intact. Without
// this, "refuse everything" would satisfy the arms above.
func TestCreateAPIKey_ValidExpiry_ReachesTheStore(t *testing.T) {
	us := createStub()
	h := keyCreateHandler(us)
	want := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	body, _ := json.Marshal(map[string]any{"name": "Key", "expiresAt": want.Format(time.RFC3339)})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys", body, "admin", "admin_user")
	if err := h.CreateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
	got := us.createKeyParams.ExpiresAt
	if got == nil {
		t.Fatal("expiresAt did not reach the store — a readable expiry was dropped")
	}
	if !got.Equal(want) {
		t.Errorf("store got %s, want %s", got.UTC(), want)
	}
}

// Omitting the field is how an operator asks for a key with no expiry. That is
// a deliberate choice and stays available; the defect was making it the answer
// to an input the caller believed WAS an expiry.
func TestCreateAPIKey_NoExpiry_StillAllowed(t *testing.T) {
	us := createStub()
	h := keyCreateHandler(us)
	body, _ := json.Marshal(map[string]any{"name": "Key"})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys", body, "admin", "admin_user")
	if err := h.CreateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
	if us.createKeyParams.ExpiresAt != nil {
		t.Error("an expiry appeared that the caller never sent")
	}
}

func TestUpdateAPIKey_UnreadableExpiry_RefusesAndChangesNothing(t *testing.T) {
	uid := "u1"
	us := &stubUserStore{updateKey: &userstore.AdminAPIKey{ID: "k1", Name: "Updated", OwnerUserID: &uid}}
	h := keyCreateHandler(us)
	// Name is valid, so before the fix this answered 200 with the rename applied
	// and the expiry silently discarded — the operator's strongest signal that
	// the expiry took was a 200.
	body, _ := json.Marshal(map[string]any{"name": "Updated", "expiresAt": "2029-13-45T00:00:00Z"})
	c, rec := adminAuthCtx(http.MethodPatch, "/api-keys/k1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.UpdateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "INVALID_EXPIRES_AT") {
		t.Errorf("code=%d body=%s want 400 INVALID_EXPIRES_AT", rec.Code, rec.Body)
	}
	if us.updateKeyParams.Name != nil {
		t.Error("the rename was applied even though the request was refused")
	}
}

func TestUpdateAPIKey_ValidExpiry_ReachesTheStore(t *testing.T) {
	uid := "u1"
	us := &stubUserStore{updateKey: &userstore.AdminAPIKey{ID: "k1", Name: "Key", OwnerUserID: &uid}}
	h := keyCreateHandler(us)
	want := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	body, _ := json.Marshal(map[string]any{"expiresAt": want.Format(time.RFC3339)})
	c, rec := adminAuthCtx(http.MethodPatch, "/api-keys/k1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.UpdateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
	got := us.updateKeyParams.ExpiresAt
	if got == nil || !got.Equal(want) {
		t.Errorf("store got %v, want %s", got, want)
	}
}

// Rotate already refused a malformed value; it now refuses a past one too, and
// this arm keeps all three paths answering alike.
func TestRotateAPIKey_UnreadableExpiry_Refused(t *testing.T) {
	us := rotateStub(&stubUserStore{})
	h := rotateHandler(us)
	body, _ := json.Marshal(map[string]any{"expiresAt": "2029-13-45T00:00:00Z"})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys/k1/rotate", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.RotateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "INVALID_EXPIRES_AT") {
		t.Errorf("code=%d body=%s want 400 INVALID_EXPIRES_AT", rec.Code, rec.Body)
	}
	if us.rotateCalled {
		t.Error("a successor was minted despite the refusal")
	}
}
