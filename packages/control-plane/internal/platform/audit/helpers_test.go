package audit

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

func newTestEcho(remoteAddr string) echo.Context {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec)
}

func TestEntryForSetsEntityTypeAndAction(t *testing.T) {
	c := newTestEcho("203.0.113.42:1234")
	e := EntryFor(c, iam.ResourceVirtualKey, iam.VerbCreate)

	if e.EntityType != "virtual-key" {
		t.Errorf("EntityType = %q; want %q", e.EntityType, "virtual-key")
	}
	if e.Action != "create" {
		t.Errorf("Action = %q; want %q", e.Action, "create")
	}
	if e.SourceIP == "" {
		t.Error("SourceIP not populated from request")
	}
}

func TestEntryForPanicsOnUndeclaredVerb(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("EntryFor did not panic on undeclared verb")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value not string: %T", r)
		}
		// Sanity: panic message names the missing verb + resource.
		if !strings.Contains(msg, "approve") || !strings.Contains(msg, "provider") {
			t.Errorf("panic message %q missing verb/resource detail", msg)
		}
	}()
	// provider does not declare VerbApprove.
	c := newTestEcho("")
	_ = EntryFor(c, iam.ResourceProvider, iam.VerbApprove)
}

// TestEntryForPopulatesActorFromAdminAuth covers the AdminAuth branch of
// EntryFor: when middleware.WithAdminAuth has attached an *auth.AdminAuth to
// the context, the constructor must copy KeyID into ActorID and KeyName into
// ActorLabel. Without this the audit log loses attribution for every admin
// API call — every prod traffic event would carry ActorID="" / ActorLabel="".
func TestEntryForPopulatesActorFromAdminAuth(t *testing.T) {
	c := newTestEcho("198.51.100.7:5555")
	middleware.WithAdminAuth(c, &auth.AdminAuth{
		KeyID:             "key-42",
		KeyName:           "admin@nexus.ai",
		AuthPrincipalType: "admin_user",
	})

	e := EntryFor(c, iam.ResourceVirtualKey, iam.VerbRevoke)

	if e.ActorID != "key-42" {
		t.Errorf("ActorID = %q; want %q", e.ActorID, "key-42")
	}
	if e.ActorLabel != "admin@nexus.ai" {
		t.Errorf("ActorLabel = %q; want %q", e.ActorLabel, "admin@nexus.ai")
	}
	if e.EntityType != "virtual-key" {
		t.Errorf("EntityType = %q; want virtual-key", e.EntityType)
	}
	if e.Action != "revoke" {
		t.Errorf("Action = %q; want revoke", e.Action)
	}
	if e.SourceIP == "" {
		t.Error("SourceIP not populated")
	}
}

// TestEntryForLeavesActorEmptyWhenContextHasNoAdminAuth pins the negative
// branch — no AdminAuth attached means ActorID/ActorLabel stay zero-value
// strings (not "<nil>", not panic). This is the unauth admin-handler case
// (e.g. health probes), which must not crash the audit constructor.
func TestEntryForLeavesActorEmptyWhenContextHasNoAdminAuth(t *testing.T) {
	c := newTestEcho("")
	e := EntryFor(c, iam.ResourceVirtualKey, iam.VerbRead)
	if e.ActorID != "" {
		t.Errorf("ActorID = %q; want empty", e.ActorID)
	}
	if e.ActorLabel != "" {
		t.Errorf("ActorLabel = %q; want empty", e.ActorLabel)
	}
}

func TestEntryForActionMatchesCatalogActionBody(t *testing.T) {
	// AC-3 alignment at the audit layer: the (EntityType, Action) pair
	// produced by EntryFor must compose into the same SIEM eventType the
	// nexus-hub classifier derives — which is EntityType + "." + Action.
	// The catalog's Action() helper returns "admin:" + that same body, so
	// stripping "admin:" gives us the SIEM key. This test pins that
	// alignment for every (resource, verb) pair in the catalog.
	c := newTestEcho("")
	for i := range iam.Catalog {
		r := &iam.Catalog[i]
		for _, v := range r.Verbs {
			e := EntryFor(c, r, v)
			siemKey := e.EntityType + "." + e.Action
			actionBody := strings.TrimPrefix(r.Action(v), "admin:")
			if siemKey != actionBody {
				t.Errorf("misalignment on (%s, %s): SIEM key = %q, action body = %q",
					r.Name, v, siemKey, actionBody)
			}
		}
	}
}
