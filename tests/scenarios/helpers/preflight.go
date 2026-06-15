package helpers

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
)

// _ ctx-unused linter quiet
var _ = context.Background

// WaitForServices blocks until all four local services (Hub :3060,
// Control Plane :3001, AI Gateway :3050, Compliance Proxy :3040) answer.
// Probes every 60 s; loops *forever* — the dev environment can take an
// unbounded amount of time to come up (parallel session rebuilding
// binaries, slow Docker boot, etc.). The user binding is: never fail
// fast on a startup race; just keep waiting.
//
// "Up" is defined permissively per service — any structured HTTP response
// (1xx-5xx) within timeout counts as up, since the harness only cares
// that the listener is accepting connections. Transport-level errors
// (connection refused, EOF, timeout) count as down.
//
// Skipped entirely when NEXUS_TEST_SKIP_PREFLIGHT=1 (helpers self-test
// runs that don't need a live environment).
//
// Memory anchor: feedback_scenario_harness_preflight_retry.
func WaitForServices(env *intg.Env) {
	if os.Getenv("NEXUS_TEST_SKIP_PREFLIGHT") == "1" {
		return
	}

	probes := []servicProbe{
		{Name: "Hub", URL: env.HubURL + "/healthz"},
		// /v1/models with no VK lets us detect "AI Gateway accepting"
		// even when auth would reject the request — any structured
		// response means the listener is up.
		{Name: "AIGw", URL: env.AIGwURL + "/v1/models"},
		// Public OAuth discovery is unauthenticated; any structured
		// response means the CP HTTP server is accepting.
		{Name: "CP", URL: env.CPURL + "/.well-known/openid-configuration"},
	}
	// The compliance proxy is a TLS-CONNECT intercept reached directly by
	// org-managed devices; in a real deployment its listener is NOT exposed
	// on the public edge (no nginx server_name, security-group closed to the
	// internet). The prod safe-e2e subset is admin-read-only and never routes
	// through the proxy, so probing it would block WaitForServices forever
	// against prod. Gate the proxy probe to non-prod-safe runs (local/dev,
	// where the proxy is loopback-reachable and some scenarios do use it).
	if os.Getenv("NEXUS_PROD_SAFE_E2E") != "1" {
		// Compliance proxy on :3040 is a transparent forward proxy; a
		// root GET returns a non-2xx body but the listener is bound.
		probes = append(probes, servicProbe{Name: "Proxy", URL: env.ProxyURL + "/"})
	}

	client := &http.Client{Timeout: 3 * time.Second}
	for tick := 1; ; tick++ {
		statuses := make([]string, 0, len(probes))
		allUp := true
		for _, p := range probes {
			up := isServiceUp(client, p.URL)
			if up {
				statuses = append(statuses, p.Name+"=up")
			} else {
				statuses = append(statuses, p.Name+"=down")
				allUp = false
			}
		}
		if allUp {
			fmt.Fprintf(os.Stderr, "harness: services ready [%s]\n", joinComma(statuses))
			return
		}
		fmt.Fprintf(os.Stderr, "harness: waiting for services tick=%d [%s] — sleeping 60s\n",
			tick, joinComma(statuses))
		time.Sleep(60 * time.Second)
	}
}

type servicProbe struct {
	Name string
	URL  string
}

// isServiceUp returns true if URL answers with any HTTP status (1xx-5xx)
// within client timeout. Transport-level errors (refused, timeout,
// connection reset) count as down.
func isServiceUp(client *http.Client, url string) bool {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode > 0
}

// joinComma is a tiny string join for status lines (avoids pulling
// strings.Join into a hot path for the sake of a one-liner).
func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// ConfigKeyServices maps a thing-config-template config_key to the set
// of service-thing types that subscribe to it. Mirrors the (type,
// config_key) rows in the thing_config_template table at the time of
// writing — kept in code so scenarios don't have to live-query the DB
// on every assertion.
//
// Each scenario that mutates a config_key should call WaitForConfigApply
// once per service URL in the corresponding set, AFTER capturing a
// pre-change baseline snapshot.
var ConfigKeyServices = map[string][]string{
	// ai-gateway-only keys
	"routing_rules":          {"ai-gateway"},
	"virtual_keys":           {"ai-gateway"},
	"providers":              {"ai-gateway"},
	"models":                 {"ai-gateway"},
	"credentials":            {"ai-gateway"},
	"credential_reliability": {"ai-gateway"},
	"ai_guard":               {"ai-gateway"},
	"cache":                  {"ai-gateway"},
	"gateway_passthrough":    {"ai-gateway"},
	"organizations":          {"ai-gateway"},
	"quota_overrides":        {"ai-gateway"},
	"quota_policies":         {"ai-gateway"},
	// shared by multiple
	"hooks":                {"ai-gateway", "compliance-proxy", "agent"},
	"streaming_compliance": {"ai-gateway", "compliance-proxy", "agent"},
	"payload_capture":      {"ai-gateway", "compliance-proxy", "agent"},
	"interception_domains": {"compliance-proxy", "agent"},
	"killswitch":           {"compliance-proxy", "agent", "ai-gateway"},
	"exemptions":           {"compliance-proxy", "agent"},
	"log_level":            {"ai-gateway", "compliance-proxy", "agent", "nexus-hub", "control-plane"},
	"observability":        {"ai-gateway", "compliance-proxy", "agent", "nexus-hub", "control-plane"},
	// compliance-proxy only
	"compliance_streaming": {"compliance-proxy"},
	"domain_allowlist":     {"compliance-proxy"},
	"onboarding":           {"compliance-proxy"},
	// agent only
	"agent_settings":   {"agent"},
	"auth":             {"agent"},
	"diag_mode":        {"agent"},
	"policy_rules":     {"agent"},
	"rgc-key":          {"agent"},
	"timing_intervals": {"agent"},
}

// MetricsURLForService returns the base URL where a service-thing
// publishes its /metrics endpoint. Agent metrics aren't exposed
// network-wide (local IPC only) — returns "" for agent so scenarios
// can skip the runtime check on that path.
func MetricsURLForService(env *intg.Env, svc string) string {
	switch svc {
	case "ai-gateway":
		return env.AIGwURL
	case "control-plane":
		return env.CPURL
	case "nexus-hub":
		return env.HubURL
	case "compliance-proxy":
		// Compliance proxy hosts its forward proxy on env.ProxyURL
		// (:3040) but publishes metrics on a separate port (:9090
		// per local dev).
		return "http://localhost:9090"
	case "agent":
		return ""
	default:
		return ""
	}
}

// WaitForConfigApply blocks until each subscriber service for the named
// config_key has reported a fresh apply since the baseline snapshot
// (per-service map of pre-change snapshots). Returns the post-change
// snapshot per service so callers can carry on with metric-delta
// assertions.
//
// Skips services with no metrics URL (e.g. agent) — those scenarios
// must verify reload via a different channel (chat behavior, audit row,
// etc.). Returns an error if any service fails to register a new apply
// within deadline.
//
// This is the **runtime-state core** for config-mutating scenarios.
// Instead of a fixed `time.Sleep(3 * time.Second)` we wait for the
// actual hot-reload signal that the service publishes via
// nexus_thingclient_config_applies_total{status="success"}.
func WaitForConfigApply(
	ctx context.Context,
	env *intg.Env,
	configKey string,
	baseline map[string]*MetricSnapshot,
	deadline time.Duration,
) (map[string]*MetricSnapshot, error) {
	services, known := ConfigKeyServices[configKey]
	if !known {
		return nil, fmt.Errorf("WaitForConfigApply: unknown config_key %q — add to ConfigKeyServices map", configKey)
	}
	post := make(map[string]*MetricSnapshot, len(services))
	stopAt := time.Now().Add(deadline)
	for _, svc := range services {
		url := MetricsURLForService(env, svc)
		if url == "" {
			// agent has no network-reachable metrics endpoint
			continue
		}
		before, ok := baseline[svc]
		if !ok || before == nil {
			return nil, fmt.Errorf("WaitForConfigApply: missing baseline for %s — call ScrapeMetrics first", svc)
		}
		beforeCount := before.Counter("nexus_thingclient_config_applies_total", map[string]string{"status": "success"})
		for {
			snap, err := ScrapeMetrics(ctx, url)
			if err != nil {
				// transient: keep trying until deadline
				if time.Now().After(stopAt) {
					return nil, fmt.Errorf("WaitForConfigApply: %s scrape failed: %w", svc, err)
				}
				time.Sleep(500 * time.Millisecond)
				continue
			}
			afterCount := snap.Counter("nexus_thingclient_config_applies_total", map[string]string{"status": "success"})
			if afterCount > beforeCount {
				post[svc] = snap
				break
			}
			if time.Now().After(stopAt) {
				return nil, fmt.Errorf("WaitForConfigApply: %s did not register a new apply for config_key=%s (count stayed at %.0f)",
					svc, configKey, beforeCount)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	return post, nil
}

// BaselineConfigApply captures pre-change snapshots for every service
// that subscribes to the given config_key. Pair with WaitForConfigApply
// for a complete "config mutation → all subscribers caught up" gate.
func BaselineConfigApply(ctx context.Context, env *intg.Env, configKey string) (map[string]*MetricSnapshot, error) {
	services, known := ConfigKeyServices[configKey]
	if !known {
		return nil, fmt.Errorf("BaselineConfigApply: unknown config_key %q", configKey)
	}
	out := make(map[string]*MetricSnapshot, len(services))
	for _, svc := range services {
		url := MetricsURLForService(env, svc)
		if url == "" {
			continue
		}
		snap, err := ScrapeMetrics(ctx, url)
		if err != nil {
			return nil, fmt.Errorf("BaselineConfigApply: %s: %w", svc, err)
		}
		out[svc] = snap
	}
	return out, nil
}
