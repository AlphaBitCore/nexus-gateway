package domain

import (
	"reflect"
	"testing"
)

func TestParseTrafficDomain(t *testing.T) {
	tests := []struct {
		in     string
		want   TrafficDomain
		wantOK bool
	}{
		{"vk", DomainVK, true},
		{"proxy", DomainProxy, true},
		{"agent", DomainAgent, true},
		{" vk ", DomainVK, true},
		{"", "", false},
		{"ai-gateway", "", false}, // DB source value — reject (UI must use domain)
		{"unknown", "", false},
		{"VK", "", false}, // case-sensitive
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := ParseTrafficDomain(tc.in)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("ParseTrafficDomain(%q) = (%q, %v); want (%q, %v)",
					tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestDBSourcesFor(t *testing.T) {
	tests := []struct {
		domain TrafficDomain
		want   []string
	}{
		{DomainVK, []string{DBSourceAIGateway}},
		{DomainProxy, []string{DBSourceComplianceProxy}},
		{DomainAgent, []string{DBSourceAgent}},
		{TrafficDomain("unknown"), nil},
	}
	for _, tc := range tests {
		t.Run(string(tc.domain), func(t *testing.T) {
			got := DBSourcesFor(tc.domain)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DBSourcesFor(%q) = %v; want %v", tc.domain, got, tc.want)
			}
		})
	}
}

func TestAllDataPlaneDBSources(t *testing.T) {
	got := AllDataPlaneDBSources()
	want := []string{DBSourceAIGateway, DBSourceComplianceProxy, DBSourceAgent}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AllDataPlaneDBSources() = %v; want %v", got, want)
	}
}

func TestAllDomains(t *testing.T) {
	got := AllDomains()
	want := []TrafficDomain{DomainVK, DomainProxy, DomainAgent}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AllDomains() = %v; want %v", got, want)
	}
}

func TestDBSourceToDomain(t *testing.T) {
	tests := []struct {
		in     string
		want   TrafficDomain
		wantOK bool
	}{
		{DBSourceAIGateway, DomainVK, true},
		{DBSourceComplianceProxy, DomainProxy, true},
		{DBSourceAgent, DomainAgent, true},
		{"admin", "", false},
		{"device-lifecycle", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := DBSourceToDomain(tc.in)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("DBSourceToDomain(%q) = (%q, %v); want (%q, %v)",
					tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// Roundtrip invariant: every domain value that maps to a DB source must map
// back to the same domain. Protects against drift when new domains are added.
func TestDomainDBRoundtrip(t *testing.T) {
	for _, d := range AllDomains() {
		dbSources := DBSourcesFor(d)
		if len(dbSources) == 0 {
			t.Errorf("domain %q has no DB sources", d)
			continue
		}
		for _, src := range dbSources {
			got, ok := DBSourceToDomain(src)
			if !ok || got != d {
				t.Errorf("roundtrip: %q → %v → (%q,%v); want %q", d, dbSources, got, ok, d)
			}
		}
	}
}
