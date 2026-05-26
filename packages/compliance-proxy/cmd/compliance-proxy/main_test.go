package main

import "testing"

// TestComposeManagementBaseURL pins the management base URL the binary reports
// as ManagementURL in thingclient.Config. Same host-collapse rules as
// composeMetricsURL but no path suffix.
func TestComposeManagementBaseURL(t *testing.T) {
	cases := []struct {
		name          string
		advertiseHost string
		bindAddr      string
		want          string
	}{
		{"empty host, port-only bind", "", ":9090", "http://127.0.0.1:9090"},
		{"wildcard ipv4 host", "0.0.0.0", ":9090", "http://127.0.0.1:9090"},
		{"explicit advertiseHost", "10.1.2.3", "0.0.0.0:9090", "http://10.1.2.3:9090"},
		{"explicit ipv6 advertiseHost", "fd00::1", ":9090", "http://[fd00::1]:9090"},
		{"hostname advertiseHost", "proxy.svc.cluster.local", ":9090", "http://proxy.svc.cluster.local:9090"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := composeManagementBaseURL(tc.advertiseHost, tc.bindAddr)
			if got != tc.want {
				t.Errorf("composeManagementBaseURL(%q, %q) = %q, want %q",
					tc.advertiseHost, tc.bindAddr, got, tc.want)
			}
		})
	}
}

// TestComposeMetricsURL pins the URL this binary advertises to Hub as its
// `metricsUrl`. Empty / wildcard advertiseHost must collapse to 127.0.0.1
// so single-host dev keeps working; the port always comes from the bind
// address so operators can change one without touching the other.
//
// Regression: a previous version called net.Dial("udp4", "8.8.8.8:80") and
// used conn.LocalAddr() to discover a "publicly reachable" IP. On dev
// boxes with compliance-proxy's own TUN intercept up that returned
// 198.18.0.1, and Hub's runtime-bridge GET hit a self-loop EOF.
func TestComposeMetricsURL(t *testing.T) {
	cases := []struct {
		name          string
		advertiseHost string
		bindAddr      string
		want          string
	}{
		{"empty host, port-only bind", "", ":9090", "http://127.0.0.1:9090/metrics"},
		{"wildcard ipv4 host", "0.0.0.0", ":9090", "http://127.0.0.1:9090/metrics"},
		{"wildcard ipv6 host", "::", ":9090", "http://127.0.0.1:9090/metrics"},
		{"explicit advertiseHost wins over wildcard bind", "10.1.2.3", "0.0.0.0:9090", "http://10.1.2.3:9090/metrics"},
		{"explicit ipv6 advertiseHost", "fd00::1", ":9090", "http://[fd00::1]:9090/metrics"},
		{"hostname advertiseHost", "metrics.svc.cluster.local", ":9090", "http://metrics.svc.cluster.local:9090/metrics"},
		{"unparseable bind falls through to a visible-on-failure URL", "", "not-a-host", "http://127.0.0.1not-a-host/metrics"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := composeMetricsURL(tc.advertiseHost, tc.bindAddr)
			if got != tc.want {
				t.Errorf("composeMetricsURL(%q, %q) = %q, want %q",
					tc.advertiseHost, tc.bindAddr, got, tc.want)
			}
		})
	}
}
