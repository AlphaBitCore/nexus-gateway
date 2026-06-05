package local

import (
	"strings"
	"testing"
)

func TestValidateBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
		// errHas, when non-empty, must be a substring of the error message.
		errHas string
	}{
		{"https FQDN", "https://nexus.taskforce10x.com", false, ""},
		{"http FQDN with port", "http://gateway.internal:3050", false, ""},
		{"localhost", "http://localhost:3001", false, ""},
		{"localhost https", "https://localhost", false, ""},
		{"loopback IP", "http://127.0.0.1:3001", false, ""},
		{"IPv6", "http://[::1]:3001", false, ""},
		{"trailing slash + path ok", "https://nexus.example.com/", false, ""},

		{"empty", "", true, "required"},
		{"whitespace only", "   ", true, "required"},
		{"the prod typo", "https://prod", true, "not reachable"},
		{"bare label http", "http://gateway", true, "not reachable"},
		{"no scheme", "nexus.taskforce10x.com", true, "http:// or https://"},
		{"wrong scheme", "ftp://nexus.example.com", true, "http:// or https://"},
		{"scheme only, no host", "https://", true, "missing a host"},
	}
	for _, c := range cases {
		err := ValidateBaseURL("Control Plane URL", c.raw)
		if c.wantErr && err == nil {
			t.Errorf("%s: expected an error for %q, got nil", c.name, c.raw)
			continue
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s: expected %q to be valid, got: %v", c.name, c.raw, err)
			continue
		}
		if err != nil && c.errHas != "" && !strings.Contains(err.Error(), c.errHas) {
			t.Errorf("%s: error %q should contain %q", c.name, err.Error(), c.errHas)
		}
	}
}
