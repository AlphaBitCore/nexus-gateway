package login

import (
	"testing"

	"github.com/crewjam/saml"
)

const emailClaimURI = "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress"

// attrAssertion builds an assertion with the given attributes (name → values)
// and an optional NameID (value + format).
func attrAssertion(nameIDValue, nameIDFormat string, attrs []saml.Attribute) *saml.Assertion {
	a := &saml.Assertion{
		AttributeStatements: []saml.AttributeStatement{{Attributes: attrs}},
	}
	if nameIDValue != "" || nameIDFormat != "" {
		a.Subject = &saml.Subject{NameID: &saml.NameID{Value: nameIDValue, Format: nameIDFormat}}
	}
	return a
}

func TestSAMLEmailWithFallback(t *testing.T) {
	emailAttr := func(name, val string) saml.Attribute {
		return saml.Attribute{Name: name, Values: []saml.AttributeValue{{Value: val}}}
	}

	tests := []struct {
		name       string
		configured string
		assertion  *saml.Assertion
		want       string
	}{
		{
			name:       "configured attribute wins",
			configured: "email",
			assertion:  attrAssertion("", "", []saml.Attribute{emailAttr("email", "primary@x")}),
			want:       "primary@x",
		},
		{
			// The Auth0 case: admin left the default "email", but Auth0 emits
			// the value under the long claim URI. Fallback resolves it.
			name:       "default missing, long claim URI via fallback",
			configured: "email",
			assertion:  attrAssertion("", "", []saml.Attribute{emailAttr(emailClaimURI, "auth0@x")}),
			want:       "auth0@x",
		},
		{
			name:       "mail attribute via fallback",
			configured: "email",
			assertion:  attrAssertion("", "", []saml.Attribute{emailAttr("mail", "ldap@x")}),
			want:       "ldap@x",
		},
		{
			// No email attribute at all, but the NameID is in emailAddress
			// format — the subject is the email of last resort.
			name:       "emailAddress-format NameID last resort",
			configured: "email",
			assertion:  attrAssertion("subject@x", string(saml.EmailAddressNameIDFormat), nil),
			want:       "subject@x",
		},
		{
			// A NameID in a non-email format must NOT be treated as an email.
			name:       "persistent NameID is not an email",
			configured: "email",
			assertion:  attrAssertion("opaque-id-123", "urn:oasis:names:tc:SAML:2.0:nameid-format:persistent", nil),
			want:       "",
		},
		{
			name:       "nothing matches",
			configured: "email",
			assertion:  attrAssertion("", "", []saml.Attribute{emailAttr("unrelated", "x")}),
			want:       "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := samlEmailWithFallback(tc.assertion, tc.configured); got != tc.want {
				t.Errorf("samlEmailWithFallback = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSAMLGroupsWithFallback(t *testing.T) {
	groupAttr := func(name string, vals ...string) saml.Attribute {
		avs := make([]saml.AttributeValue, len(vals))
		for i, v := range vals {
			avs[i] = saml.AttributeValue{Value: v}
		}
		return saml.Attribute{Name: name, Values: avs}
	}

	t.Run("configured attribute wins", func(t *testing.T) {
		a := attrAssertion("", "", []saml.Attribute{groupAttr("groups", "admins", "devs")})
		got := samlGroupsWithFallback(a, "groups")
		if len(got) != 2 || got[0] != "admins" || got[1] != "devs" {
			t.Errorf("got %v, want [admins devs]", got)
		}
	})

	t.Run("memberOf via fallback when default groups missing", func(t *testing.T) {
		a := attrAssertion("", "", []saml.Attribute{groupAttr("memberOf", "cn=staff")})
		got := samlGroupsWithFallback(a, "groups")
		if len(got) != 1 || got[0] != "cn=staff" {
			t.Errorf("got %v, want [cn=staff]", got)
		}
	})

	t.Run("Azure AD groups claim URI via fallback", func(t *testing.T) {
		a := attrAssertion("", "", []saml.Attribute{groupAttr("http://schemas.microsoft.com/ws/2008/06/identity/claims/groups", "g1")})
		got := samlGroupsWithFallback(a, "groups")
		if len(got) != 1 || got[0] != "g1" {
			t.Errorf("got %v, want [g1]", got)
		}
	})

	t.Run("nil when no group attribute present", func(t *testing.T) {
		a := attrAssertion("", "", []saml.Attribute{groupAttr("unrelated", "x")})
		if got := samlGroupsWithFallback(a, "groups"); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
}
