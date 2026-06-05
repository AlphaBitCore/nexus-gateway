package login

import (
	"net/url"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

func TestSAMLSSOURLWithParams(t *testing.T) {
	const sso = "https://dev-x.auth0.com/samlp/abc123"

	t.Run("no params returns url unchanged", func(t *testing.T) {
		got, err := samlSSOURLWithParams(sso, nil)
		if err != nil || got != sso {
			t.Fatalf("got %q, err %v; want unchanged", got, err)
		}
	})

	t.Run("appends configured param (Auth0 organization)", func(t *testing.T) {
		got, err := samlSSOURLWithParams(sso, []store.SAMLSSOParam{{Key: "organization", Value: "org_abc123"}})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		u, _ := url.Parse(got)
		if u.Query().Get("organization") != "org_abc123" {
			t.Errorf("organization not appended: %q", got)
		}
		// Path + host preserved.
		if u.Host != "dev-x.auth0.com" || u.Path != "/samlp/abc123" {
			t.Errorf("endpoint mangled: %q", got)
		}
	})

	t.Run("preserves a pre-existing query param on the SSO URL", func(t *testing.T) {
		got, err := samlSSOURLWithParams(sso+"?connection=Username-Password", []store.SAMLSSOParam{{Key: "organization", Value: "o1"}})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		u, _ := url.Parse(got)
		if u.Query().Get("connection") != "Username-Password" || u.Query().Get("organization") != "o1" {
			t.Errorf("params not merged: %q", got)
		}
	})

	t.Run("skips empty-key and reserved SAML params", func(t *testing.T) {
		got, err := samlSSOURLWithParams(sso, []store.SAMLSSOParam{
			{Key: "", Value: "ignored"},
			{Key: "SAMLRequest", Value: "evil"},
			{Key: "RelayState", Value: "evil"},
			{Key: "organization", Value: "ok"},
		})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		u, _ := url.Parse(got)
		q := u.Query()
		if q.Has("SAMLRequest") || q.Has("RelayState") {
			t.Errorf("reserved SAML params must not be injectable: %q", got)
		}
		if q.Get("organization") != "ok" {
			t.Errorf("valid param dropped: %q", got)
		}
	})

	t.Run("unparseable url errors", func(t *testing.T) {
		if _, err := samlSSOURLWithParams("https://%zz", []store.SAMLSSOParam{{Key: "k", Value: "v"}}); err == nil {
			t.Error("expected error for unparseable SSO URL")
		}
	})
}
