// Daemon-bound — Compliance Proxy CONNECT (TLS-bump) pipeline.
//
// The Compliance Proxy MITM-intercepts HTTPS provider traffic on its CONNECT
// port, applies the compliance pipeline, and writes a traffic_event row with
// source='compliance-proxy'. This scenario drives one bounded request through
// the real CONNECT path and asserts the bumped row lands. It is the live
// counterpart of the admin-side proxy scenarios (S-085) and the runtime arm of
// P3 (daemon in CI).
package scenarios_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
)

// TestS083_ComplianceProxyConnectPipeline — daemon-bound CONNECT scenario.
//
// Preconditions (each a documented SKIP, never a red — a missing local CA or
// provider key is an env gap, not our bug):
//   - the proxy CONNECT endpoint (NEXUS_CP_PROXY_ADDR, default localhost:3128),
//   - the proxy dev CA cert (NEXUS_CP_PROXY_CA, default the repo dev-certs path),
//   - a plaintext provider key + OpenAI-compatible base URL + model
//     (NEXUS_PROXY_TEST_KEY / NEXUS_PROXY_TEST_BASEURL / NEXUS_PROXY_TEST_MODEL).
//     CI supplies these as secrets; locally an operator exports them.
//
// When met: one non-stream chat is sent THROUGH the proxy (HTTP CONNECT + the
// proxy's bumped TLS), and a traffic_event row with source='compliance-proxy'
// and a matching target_host must land.
func TestS083_ComplianceProxyConnectPipeline(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	proxyAddr := getenvDefault("NEXUS_CP_PROXY_ADDR", "localhost:3128")
	// The default is resolved from the REPO ROOT, not the process CWD. As a
	// relative path it only worked when `go test` happened to run from the repo
	// root; from tests/scenarios/ — where this suite actually runs — it never
	// resolved, so the scenario skipped on "CA not readable" even with the CA
	// sitting in the tree.
	caPath := os.Getenv("NEXUS_CP_PROXY_CA")
	if caPath == "" {
		root, rootErr := intg.RepoRoot()
		if rootErr != nil {
			t.Skipf("cannot locate the repo root to resolve the proxy dev CA: %v", rootErr)
		}
		caPath = filepath.Join(root, "packages", "compliance-proxy", "dev-certs", "ca.crt")
	}
	// Resolved through Env, not os.Getenv. LoadEnv reads tests/.env.<target> into
	// a private map and never exports it to the process environment, so the
	// os.Getenv form this used could only ever see a var an operator had exported
	// by hand — and the doc comment duly said "locally an operator exports them".
	// Nobody does, so this scenario skipped on every local run since it was
	// written while tests/scripts/bump-regression.py, reading OPENAI_API_KEY from
	// the same file through the SHELL loader (which does export), exercised the
	// same proxy happily. Env.ProxyBumpKey now resolves NEXUS_PROXY_TEST_KEY first
	// and falls back to that same OPENAI_API_KEY.
	provKey := sc.Env.ProxyBumpKey
	baseURL := sc.Env.ProxyBumpBaseURL
	model := sc.Env.ProxyBumpModel

	if provKey == "" {
		t.Skip("compliance-proxy CONNECT pipeline needs a plaintext provider key: set " +
			"OPENAI_API_KEY in tests/.env.local (the same variable bump-regression.py uses), " +
			"or NEXUS_PROXY_TEST_KEY in the environment to override")
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		t.Skipf("proxy dev CA cert not readable at %s (set NEXUS_CP_PROXY_CA): %v — "+
			"the running proxy's CA lives in its own checkout's packages/compliance-proxy/dev-certs/", caPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("proxy CA cert at %s is not valid PEM", caPath)
	}

	// HTTP client that routes through the proxy's CONNECT and trusts its bumped TLS.
	proxyURL := &url.URL{Scheme: "http", Host: proxyAddr}
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}

	nonce := time.Now().UnixNano()
	body := mustMarshal(t, map[string]any{
		"model":       model,
		"messages":    []map[string]string{{"role": "user", "content": fmt.Sprintf("Reply OK. proxy-connect n=%d", nonce)}},
		"max_tokens":  8,
		"temperature": 0,
	})
	endpoint := strings.TrimRight(baseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+provKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request through proxy %s failed: %v (CONNECT/TLS-bump path)", proxyAddr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		// Not every non-200 is "their problem". A 404 means we asked for a path
		// that does not exist — a fixture or base-URL error on OUR side — and
		// skipping on it hides exactly that mistake: a base URL missing its /v1
		// produced a 404 here and the scenario filed it as a provider/credential
		// issue. Same for 400 (a malformed body we built).
		//
		// Genuinely amber: 401/403 (credential), 429 (their rate limit), 5xx
		// (their outage). Those say nothing about the bump pipeline, which by this
		// point has already proven itself — the CONNECT completed, the bumped TLS
		// handshake verified against the dev CA, and the request reached the
		// upstream to be answered at all.
		switch {
		case resp.StatusCode == http.StatusUnauthorized,
			resp.StatusCode == http.StatusForbidden,
			resp.StatusCode == http.StatusTooManyRequests,
			resp.StatusCode >= 500:
			// Name the credential's PROVENANCE. A 401 here says nothing about the
			// bump; what it usually says is that the key came from somewhere other
			// than the file you just edited.
			t.Skipf("upstream returned HTTP %d through the proxy — infra-amber (provider credential / rate limit / outage), not a proxy-pipeline failure. Credential source: %s",
				resp.StatusCode, sc.Env.ProxyBumpKeySource)
		default:
			t.Fatalf("upstream returned HTTP %d for %s through the proxy — a 4xx that is not 401/403/429 means OUR request was wrong (path, base URL or body), not the provider's", resp.StatusCode, endpoint)
		}
	}

	// Assert the bumped row landed with source='compliance-proxy'.
	host := proxyTargetHost(baseURL)
	const query = `
		SELECT count(*)
		FROM traffic_event
		WHERE source = 'compliance-proxy'
		  AND target_host LIKE $1
		  AND "timestamp" > NOW() - INTERVAL '300 seconds'`
	const tries = 15
	const interval = 2 * time.Second
	var n int64
	for i := 0; i < tries; i++ {
		if scanErr := sc.DB.QueryRow(ctx, query, "%"+host+"%").Scan(&n); scanErr == nil && n >= 1 {
			break
		}
		time.Sleep(interval)
	}
	if n < 1 {
		t.Fatalf("no traffic_event row with source='compliance-proxy' target_host~%q within %v — "+
			"the CONNECT/TLS-bump pipeline did not record the bumped request", host, time.Duration(tries)*interval)
	}
	t.Logf("S-083 OK: bumped CONNECT request recorded — source='compliance-proxy' target_host~%q rows=%d", host, n)
}

func getenvDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// proxyTargetHost extracts the host (no scheme/port) from a base URL for the
// traffic_event.target_host LIKE match.
func proxyTargetHost(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return strings.TrimRight(strings.TrimPrefix(strings.TrimPrefix(baseURL, "https://"), "http://"), "/")
	}
	return u.Hostname()
}
