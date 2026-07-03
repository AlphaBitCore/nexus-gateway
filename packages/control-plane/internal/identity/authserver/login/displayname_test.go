package login

import (
	"encoding/base64"
	"github.com/goccy/go-json"
	"testing"

	"github.com/crewjam/saml"
)

func TestSAMLNameWithFallback(t *testing.T) {
	const fullNameURI = "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name"
	const givenURI = "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/givenname"
	const surnameURI = "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/surname"

	tests := []struct {
		name  string
		attrs []saml.Attribute
		email string
		want  string
	}{
		{"full-name claim URI wins", []saml.Attribute{attr(fullNameURI, "Alice Cooper")}, "", "Alice Cooper"},
		{"displayName attribute", []saml.Attribute{attr("displayName", "Bob Dylan")}, "", "Bob Dylan"},
		{"given + family composed", []saml.Attribute{attr(givenURI, "Carol"), attr(surnameURI, "King")}, "", "Carol King"},
		{"given only", []saml.Attribute{attr("givenName", "Dave")}, "", "Dave"},
		{"no name + no email → empty", []saml.Attribute{attr("dept", "eng")}, "", ""},
		{"email-valued name falls to humanized nickname", []saml.Attribute{attr(fullNameURI, "steve.chen@alphabitcore.com"), attr("http://schemas.auth0.com/nickname", "steve.chen")}, "steve.chen@alphabitcore.com", "steve chen"},
		{"email-valued name, no nickname → humanized email local-part", []saml.Attribute{attr(fullNameURI, "steve.chen@alphabitcore.com")}, "steve.chen@alphabitcore.com", "steve chen"},
		{"nickname handle when no full/given name", []saml.Attribute{attr("nickname", "ada.lovelace")}, "", "ada lovelace"},
		{"no name attribute → humanized email local-part", []saml.Attribute{attr("dept", "eng")}, "grace.hopper@navy.mil", "grace hopper"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &saml.Assertion{AttributeStatements: []saml.AttributeStatement{{Attributes: tc.attrs}}}
			if got := samlNameWithFallback(a, tc.email); got != tc.want {
				t.Errorf("samlNameWithFallback = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHumanizeHandle(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"steve.chen", "steve chen"},
		{"steve.chen@alphabitcore.com", "steve chen"},
		{"steve_chen", "steve chen"},
		{"john.doe.42", "john doe"},
		{"gracehopper", "gracehopper"},
		{"", ""},
		{"@only-domain.com", ""},
		{"123", ""},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := humanizeHandle(tc.in); got != tc.want {
				t.Errorf("humanizeHandle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestOIDCDisplayName(t *testing.T) {
	mkToken := func(claims map[string]any) string {
		b, _ := json.Marshal(claims)
		return "header." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
	}

	tests := []struct {
		name    string
		claims  map[string]any
		email   string
		subject string
		want    string
	}{
		{"name claim wins", map[string]any{"name": "Eve Online", "email": "eve@x"}, "eve@x", "sub-1", "Eve Online"},
		{"given+family when no name", map[string]any{"given_name": "Frank", "family_name": "Ocean"}, "f@x", "sub-2", "Frank Ocean"},
		{"preferred_username humanized when no name/given", map[string]any{"preferred_username": "grace.hopper"}, "g@x", "sub-3", "grace hopper"},
		{"humanized email local-part when no name claims", map[string]any{}, "harry.styles@x.com", "sub-4", "harry styles"},
		{"falls back to subject when no name + no email", map[string]any{}, "", "sub-5", "sub-5"},
		{"email-valued name falls to humanized preferred_username", map[string]any{"name": "steve.chen@alphabitcore.com", "preferred_username": "steve.chen"}, "steve.chen@alphabitcore.com", "sub-6", "steve chen"},
		{"email-valued name falls to humanized nickname", map[string]any{"name": "steve.chen@alphabitcore.com", "nickname": "steve.chen"}, "steve.chen@alphabitcore.com", "sub-7", "steve chen"},
		{"email-valued name, no handles → humanized email local-part", map[string]any{"name": "steve.chen@alphabitcore.com"}, "steve.chen@alphabitcore.com", "sub-8", "steve chen"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := oidcDisplayName(mkToken(tc.claims), tc.email, tc.subject); got != tc.want {
				t.Errorf("oidcDisplayName = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("malformed token falls back to humanized email local-part", func(t *testing.T) {
		if got := oidcDisplayName("not-a-jwt", "fallback.user@x", "sub"); got != "fallback user" {
			t.Errorf("got %q, want %q", got, "fallback user")
		}
	})
}
