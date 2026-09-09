package iam

import (
	"sort"
	"testing"
)

func TestNormalisePrincipalType(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
		why    string
	}{
		{"admin_user", "nexus_user", true,
			"the session spelling for a dashboard login; IAM storage records the same principal as nexus_user"},
		{"nexus_user", "nexus_user", true,
			"already canonical — normalising must not disturb the spelling that already works"},
		{"api_key", "api_key", true,
			"canonical in both vocabularies; there is nothing to translate"},
		{"nexus_usr", "", false,
			"one letter short. The column has no constraint, so storing this makes a row that is real, listable, and can never be loaded back"},
		{"", "", false,
			"an absent type is not a licence to guess one"},
		{"NEXUS_USER", "", false,
			"case matters: LoadPolicies compares the column for equality, so a differently-cased row is a different row"},
	}
	for _, tc := range cases {
		got, ok := NormalisePrincipalType(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("NormalisePrincipalType(%q) = (%q, %v), want (%q, %v) — %s",
				tc.in, got, ok, tc.want, tc.wantOK, tc.why)
		}
	}
}

// Every accepted spelling must actually be accepted. Without this the helper
// could drift into advertising a value the normaliser rejects, and the error
// message would name a fix that does not work.
func TestAcceptedPrincipalTypesAreAllAccepted(t *testing.T) {
	for _, pt := range AcceptedPrincipalTypes() {
		if _, ok := NormalisePrincipalType(pt); !ok {
			t.Errorf("AcceptedPrincipalTypes lists %q but NormalisePrincipalType rejects it", pt)
		}
	}
}

// And the converse: every canonical type and every alias must be listed, or an
// error message tells an operator to use a spelling it never mentions.
func TestAcceptedPrincipalTypesIsComplete(t *testing.T) {
	want := []string{}
	for c := range CanonicalPrincipalTypes {
		want = append(want, c)
	}
	for a := range principalTypeAliases {
		want = append(want, a)
	}
	sort.Strings(want)
	got := append([]string(nil), AcceptedPrincipalTypes()...)
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("AcceptedPrincipalTypes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AcceptedPrincipalTypes = %v, want %v", got, want)
		}
	}
}

// An alias must never also be canonical: storage would then hold two spellings
// for one principal, which is the divergence this file exists to end.
func TestNoAliasIsAlsoCanonical(t *testing.T) {
	for alias := range principalTypeAliases {
		if _, isCanonical := CanonicalPrincipalTypes[alias]; isCanonical {
			t.Errorf("%q is both an alias and canonical; storage would hold two spellings "+
				"for the same principal", alias)
		}
	}
}

// Every alias must resolve to something canonical, or normalising produces a
// value nothing will ever match.
func TestEveryAliasResolvesToACanonicalType(t *testing.T) {
	for alias, target := range principalTypeAliases {
		if _, ok := CanonicalPrincipalTypes[target]; !ok {
			t.Errorf("alias %q maps to %q, which is not canonical — LoadPolicies would "+
				"match nothing", alias, target)
		}
	}
}
