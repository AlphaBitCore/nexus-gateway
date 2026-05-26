package helpers

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"time"
)

// LocalHTTPClient returns an *http.Client that ignores the OS-level proxy
// configuration. The shell smoke scripts use curl which never reads
// HTTP_PROXY by default; the Python tests pass trust_env=False; this Go
// helper mirrors that contract so a workstation HTTP_PROXY pointing at,
// say, 127.0.0.1:10080 (a common setup for routing dev traffic) does not
// silently rewrite localhost requests into 502s. The trap is documented
// in tests/e2e-python/ai_judge/judge.py for the same reason.
func LocalHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			// Returning nil here means "don't use any proxy" regardless
			// of the environment.
			Proxy: func(*http.Request) (*url.URL, error) { return nil, nil },
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        16,
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			DisableCompression:  false,
		},
	}
}

// DoJSON is a tiny wrapper that adds a context, an Authorization header,
// and Content-Type: application/json when a non-nil body is provided.
// Returns (status, body bytes, error). Use it from tests when the
// dedicated provider helpers (AIGwPostJSON, CPGet, ...) are too coarse.
func DoJSON(client *http.Client, ctx context.Context, method, url string, auth string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytesReader(body))
	if err != nil {
		return 0, nil, err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := readAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, out, nil
}
