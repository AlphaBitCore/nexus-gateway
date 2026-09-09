package iam

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The defect, pinned in its own name.
//
// The console signs in as "admin_user" and the attach URL carries that same
// spelling, but IAM storage records the principal as "nexus_user" and
// LoadPolicies matches the column for EQUALITY. Storing what the URL said
// produced a row that was real, listed by the read path — which read by the
// same parameter — and invisible to every evaluation. The operator saw the
// grant; the request was still refused.
func TestAttachPrincipalPolicy_SessionSpellingIsStoredCanonically(t *testing.T) {
	is := &stubIAMStore{attachPPID: "att-1"}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"policyId": "p1"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/principals/admin_user/u1/policies", body, "admin", "admin_user")
	c.SetParamNames("type", "id")
	c.SetParamValues("admin_user", "u1")

	if err := h.AttachPrincipalPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
	if is.gotAttachPT != "nexus_user" {
		t.Errorf("stored principalType = %q, want \"nexus_user\" — an attachment written "+
			"under the session's own spelling is listable and never loaded, because "+
			"LoadPolicies compares the column for equality", is.gotAttachPT)
	}
}

// A type that is already canonical must pass through untouched, or normalising
// would break the spelling that already works.
func TestAttachPrincipalPolicy_CanonicalSpellingIsUnchanged(t *testing.T) {
	is := &stubIAMStore{attachPPID: "att-1"}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"policyId": "p1"})
	c, _ := adminAuthCtx(http.MethodPost, "/iam/principals/nexus_user/u1/policies", body, "admin", "admin_user")
	c.SetParamNames("type", "id")
	c.SetParamValues("nexus_user", "u1")

	if err := h.AttachPrincipalPolicy(c); err != nil {
		t.Fatal(err)
	}
	if is.gotAttachPT != "nexus_user" {
		t.Errorf("stored principalType = %q, want \"nexus_user\" unchanged", is.gotAttachPT)
	}
}

// A write path must REFUSE a type nothing recognises rather than store it. The
// column has no constraint, so an unrecognised value produces a row that is
// real, listable, and permanently inert — the same silent shape as the defect
// above, reached a different way.
func TestAttachPrincipalPolicy_UnknownTypeIsRefusedNotStored(t *testing.T) {
	is := &stubIAMStore{attachPPID: "att-1"}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"policyId": "p1"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/principals/nexus_usr/u1/policies", body, "admin", "admin_user")
	c.SetParamNames("type", "id")
	c.SetParamValues("nexus_usr", "u1") // one letter short

	if err := h.AttachPrincipalPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 — an unstorable principal type must be refused", rec.Code)
	}
	if is.gotAttachPT != "" {
		t.Errorf("nothing may reach the store on refusal; got %q", is.gotAttachPT)
	}
}

// The second write path, same defect. nexus-hub's smart_group.go had to query
// `principalType IN ('nexus_user', 'admin_user')` to cope with rows this path
// already let through — that workaround is the evidence it was reachable.
func TestAddIAMGroupMember_SessionSpellingIsStoredCanonically(t *testing.T) {
	is := &stubIAMStore{memberID: "mem-1"}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"principalType": "admin_user", "principalId": "u1"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/groups/g1/members", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")

	if err := h.AddIAMGroupMember(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
	if is.gotMemberPT != "nexus_user" {
		t.Errorf("stored principalType = %q, want \"nexus_user\" — group-inherited policies "+
			"load through the same equality match", is.gotMemberPT)
	}
}

func TestAddIAMGroupMember_UnknownTypeIsRefusedNotStored(t *testing.T) {
	is := &stubIAMStore{memberID: "mem-1"}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"principalType": "robot", "principalId": "u1"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/groups/g1/members", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")

	if err := h.AddIAMGroupMember(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400", rec.Code)
	}
	if is.gotMemberPT != "" {
		t.Errorf("nothing may reach the store on refusal; got %q", is.gotMemberPT)
	}
}
