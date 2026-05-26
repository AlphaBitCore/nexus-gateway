package main

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/cmd/ai-gateway/wiring"
)

// TestDefaultAdvertiseHost pins the host-resolution behaviour for the URL
// this binary advertises to Hub as its `metricsUrl`. Empty / wildcard
// values must collapse to 127.0.0.1 so single-host dev keeps working;
// explicit values pass through so multi-host operators can point Hub at
// a routable address.
func TestDefaultAdvertiseHost(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "127.0.0.1"},
		{"ipv4 wildcard", "0.0.0.0", "127.0.0.1"},
		{"ipv6 wildcard", "::", "127.0.0.1"},
		{"explicit ipv4", "10.1.2.3", "10.1.2.3"},
		{"explicit hostname", "ai-gateway.svc.cluster.local", "ai-gateway.svc.cluster.local"},
		{"localhost is taken at face value", "localhost", "localhost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wiring.DefaultAdvertiseHost(tc.in); got != tc.want {
				t.Errorf("wiring.DefaultAdvertiseHost(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
