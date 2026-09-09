package iam

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
)

// The defect, pinned in its own name.
//
// NexusUser.organizationId carries `@default("default")` and a foreign key to
// Organization.id, but the seed has never created an organisation with that id
// — it seeds a UUID whose CODE is "DEFAULT". So the column default could not be
// satisfied on any deployment, fresh or otherwise, and a create that relied on
// it tripped the foreign key every time. SCIM, OIDC-JIT and agent enrollment
// all resolve the organisation instead; the admin create path was the one that
// did not.
func TestCreateUser_OmittedOrganisationIsResolvedNotLeftToTheColumnDefault(t *testing.T) {
	us := &stubUserStore{
		defaultOrgID: "org-resolved",
		createResult: &userstore.NexusUserSafe{ID: "u-1", DisplayName: "u", Status: "active"},
	}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"username": "u", "password": "secure-pass123"})
	c, rec := adminAuthCtx(http.MethodPost, "/users", body, "admin", "admin_user")

	if err := h.CreateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
	if us.gotCreateOrgID == nil {
		t.Fatal("no organizationId reached the store — the create would fall back to a " +
			"column default that no Organization row can satisfy")
	}
	if *us.gotCreateOrgID != "org-resolved" {
		t.Errorf("organizationId = %q, want the resolved default %q", *us.gotCreateOrgID, "org-resolved")
	}
}

// An explicit organisation must win. Resolving over the caller's own value
// would be the silent-rewrite this codebase refuses on principle.
func TestCreateUser_ExplicitOrganisationIsNotOverridden(t *testing.T) {
	us := &stubUserStore{
		defaultOrgID: "org-resolved",
		createResult: &userstore.NexusUserSafe{ID: "u-1", DisplayName: "u", Status: "active"},
	}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"username": "u", "password": "secure-pass123", "organizationId": "org-chosen"})
	c, _ := adminAuthCtx(http.MethodPost, "/users", body, "admin", "admin_user")

	if err := h.CreateUser(c); err != nil {
		t.Fatal(err)
	}
	if us.gotCreateOrgID == nil || *us.gotCreateOrgID != "org-chosen" {
		got := "<nil>"
		if us.gotCreateOrgID != nil {
			got = *us.gotCreateOrgID
		}
		t.Errorf("organizationId = %q, want the caller's own %q", got, "org-chosen")
	}
}

// A deployment with no organisation at all must say so. Letting the insert fail
// on the foreign key reported "a referenced record does not exist (check
// organizationId)" to a caller who referenced nothing.
func TestCreateUser_NoOrganisationToDefaultToIsAClearRefusal(t *testing.T) {
	us := &stubUserStore{createResult: &userstore.NexusUserSafe{ID: "u-1"}} // defaultOrgID zero
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"username": "u", "password": "secure-pass123"})
	c, rec := adminAuthCtx(http.MethodPost, "/users", body, "admin", "admin_user")

	if err := h.CreateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "NO_DEFAULT_ORGANIZATION") {
		t.Errorf("the refusal must name its own cause; got %s", rec.Body)
	}
	if us.gotCreateOrgID != nil {
		t.Error("nothing may reach the store when there is no organisation to use")
	}
}

// A resolver failure is a server error, not a validation one — the caller did
// nothing wrong and retrying the same request is reasonable.
func TestCreateUser_ResolverFailureIs500(t *testing.T) {
	us := &stubUserStore{defaultOrgErr: errors.New("connection reset")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"username": "u", "password": "secure-pass123"})
	c, rec := adminAuthCtx(http.MethodPost, "/users", body, "admin", "admin_user")

	if err := h.CreateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}
