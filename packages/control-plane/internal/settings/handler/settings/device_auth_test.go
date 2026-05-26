package settings

import (
	"reflect"
	"testing"

	authserver_store "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

func TestIsValidDeviceAuthMode(t *testing.T) {
	cases := []struct {
		mode string
		want bool
	}{
		{"mtls-only", true},
		{"enterprise-login", true},
		{"local-login", true},
		{"", false},
		{"MTLS-ONLY", false},   // case-sensitive
		{"local", false},       // close-but-wrong
		{"sso", false},         // wrong word
		{"local_login", false}, // wrong separator
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			if got := isValidDeviceAuthMode(tc.mode); got != tc.want {
				t.Errorf("isValidDeviceAuthMode(%q) = %v, want %v", tc.mode, got, tc.want)
			}
		})
	}
}

func TestCategoriseDeviceAuthIdPs(t *testing.T) {
	cases := []struct {
		name string
		idps []authserver_store.IdentityProvider
		want deviceAuthSettings
	}{
		{
			name: "empty list — nothing available",
			idps: nil,
			want: deviceAuthSettings{
				SsoConfigured:       false,
				LocalLoginAvailable: false,
				SsoProviders:        []map[string]string{},
			},
		},
		{
			name: "only local — local-login available, enterprise not",
			idps: []authserver_store.IdentityProvider{
				{ID: "00000000-0000-0000-0000-000000000001", Type: "local", Name: "Nexus Local"},
			},
			want: deviceAuthSettings{
				SsoConfigured:       false,
				LocalLoginAvailable: true,
				SsoProviders:        []map[string]string{},
			},
		},
		{
			name: "only oidc — enterprise available, local-login not",
			idps: []authserver_store.IdentityProvider{
				{ID: "okta-uuid", Type: "oidc", Name: "Okta"},
			},
			want: deviceAuthSettings{
				SsoConfigured:       true,
				LocalLoginAvailable: false,
				SsoProviders: []map[string]string{
					{"id": "okta-uuid", "type": "oidc", "name": "Okta"},
				},
			},
		},
		{
			name: "both — both modes available, local omitted from provider list",
			idps: []authserver_store.IdentityProvider{
				{ID: "local-uuid", Type: "local", Name: "Nexus Local"},
				{ID: "okta-uuid", Type: "oidc", Name: "Okta"},
				{ID: "saml-uuid", Type: "saml", Name: "AzureAD"},
			},
			want: deviceAuthSettings{
				SsoConfigured:       true,
				LocalLoginAvailable: true,
				SsoProviders: []map[string]string{
					{"id": "okta-uuid", "type": "oidc", "name": "Okta"},
					{"id": "saml-uuid", "type": "saml", "name": "AzureAD"},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := categoriseDeviceAuthIdPs(tc.idps)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("categoriseDeviceAuthIdPs() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
