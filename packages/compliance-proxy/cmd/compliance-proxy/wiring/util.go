package wiring

import (
	"net"
	"time"
)

// ParseDurationOrDefault parses s as a duration; returns def on empty/error.
func ParseDurationOrDefault(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

// ExtractDomains extracts bare hostnames from domain:port allowlist entries
// for cert pre-warming.
func ExtractDomains(entries []string) []string {
	seen := make(map[string]bool)
	var domains []string
	for _, entry := range entries {
		host := entry
		if h, _, err := net.SplitHostPort(entry); err == nil {
			host = h
		}
		if len(host) > 2 && host[:2] == "*." {
			host = host[2:]
		}
		if host != "" && !seen[host] {
			seen[host] = true
			domains = append(domains, host)
		}
	}
	return domains
}

// ComposeMetricsURL returns the URL Hub uses to reach this binary's
// /metrics + /debug/runtime endpoints (registered via thingclient as
// `metricsUrl`).
func ComposeMetricsURL(advertiseHost, bindAddr string) string {
	host := advertiseHost
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	_, port, err := net.SplitHostPort(bindAddr)
	if err != nil || port == "" {
		return "http://" + host + bindAddr + "/metrics"
	}
	return "http://" + net.JoinHostPort(host, port) + "/metrics"
}

// ComposeManagementBaseURL returns the base URL of the management HTTP server
// (same host:port as the metrics server, path-free).
func ComposeManagementBaseURL(advertiseHost, bindAddr string) string {
	host := advertiseHost
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	_, port, err := net.SplitHostPort(bindAddr)
	if err != nil || port == "" {
		return "http://" + host + bindAddr
	}
	return "http://" + net.JoinHostPort(host, port)
}
