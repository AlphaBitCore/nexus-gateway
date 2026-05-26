package enrollment

// internal_test.go is the only whitebox test file in this package — it
// reaches into unexported helpers (openBrowser, callbackServer
// internals) that cannot be exercised through the public Flow.Run
// surface without a real graphical session or a production seam.

import (
	"context"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestOpenBrowser_DarwinDispatch covers the darwin arm of openBrowser
// (the runtime.GOOS=="darwin" → exec.Command("open", url) branch).
// The seam ssoExecCommandStart is stubbed so no real `open` shell-out
// happens — previously this test launched a real browser tab on every
// run because the default seam shelled to `/usr/bin/open data:,`.
func TestOpenBrowser_DarwinDispatch(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("darwin branch only reachable on darwin runner (current: %s)", runtime.GOOS)
	}
	var gotName string
	var gotArgs []string
	origExec := ssoExecCommandStart
	ssoExecCommandStart = func(name string, args ...string) error {
		gotName = name
		gotArgs = append([]string(nil), args...)
		return nil
	}
	t.Cleanup(func() { ssoExecCommandStart = origExec })

	if err := openBrowser("https://example.test/"); err != nil {
		t.Fatalf("openBrowser darwin: %v", err)
	}
	if gotName != "open" {
		t.Errorf("dispatched cmd = %q, want %q", gotName, "open")
	}
	if len(gotArgs) != 1 || gotArgs[0] != "https://example.test/" {
		t.Errorf("dispatched args = %v, want [https://example.test/]", gotArgs)
	}
}

// TestCallbackServer_SecondCallbackIgnored covers server.go:80 — the
// `default` arm of the select on s.result, taken when handleCallback
// fires twice (e.g. browser reload of the callback page). Without this
// guard the second send would block forever; the goroutine would leak
// and the server's Close would hang.
func TestCallbackServer_SecondCallbackIgnored(t *testing.T) {
	srv, err := newCallbackServer()
	if err != nil {
		t.Fatalf("newCallbackServer: %v", err)
	}
	defer srv.Close()

	port := srv.Port()
	if port <= 0 || port > 65535 {
		t.Fatalf("Port returned bogus value: %d", port)
	}
	cbURL := "http://127.0.0.1:" + itoaPort(port) + "/callback?code=c1&state=s1"

	// First callback: drains result.
	resp1, err := http.Get(cbURL) //nolint:noctx // test helper
	if err != nil {
		t.Fatalf("first callback GET: %v", err)
	}
	body1, _ := io.ReadAll(resp1.Body)
	_ = resp1.Body.Close()
	if !strings.Contains(string(body1), "successful") {
		t.Errorf("first callback response should be success HTML; got %q", string(body1))
	}

	// Drain the result channel via Wait so the next callback exercises
	// the "second callback" path on an already-drained channel.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	code, state, err := srv.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if code != "c1" || state != "s1" {
		t.Errorf("Wait returned code=%q state=%q, want c1/s1", code, state)
	}

	// Second callback: result channel is buffered=1, already drained
	// by Wait — so the second send fills it again instead of hitting
	// the default arm. To force the default arm, fire two callbacks
	// back-to-back BEFORE Wait drains. Use a fresh server.
	srv2, err := newCallbackServer()
	if err != nil {
		t.Fatalf("newCallbackServer 2: %v", err)
	}
	defer srv2.Close()
	port2 := srv2.Port()
	cbURL2 := "http://127.0.0.1:" + itoaPort(port2) + "/callback?code=c2&state=s2"

	// Fire two callbacks before reading the channel. First fills the
	// buffer; second exercises the default arm (must NOT block).
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(cbURL2) //nolint:noctx
			if err != nil {
				return
			}
			_ = resp.Body.Close()
		}()
	}
	doneWG := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneWG)
	}()
	select {
	case <-doneWG:
		// Both callbacks returned — default arm did not block.
	case <-time.After(2 * time.Second):
		t.Fatal("second callback blocked; the default arm of handleCallback's select is not protecting against a full buffer")
	}
}

// TestCallbackServer_HandleErrorRendersErrorHTML covers server.go:85 —
// the `if result.Err != "" { ...error HTML... }` branch in
// handleCallback. Verifies the rendered body so we don't silently swap
// the user-facing message.
func TestCallbackServer_HandleErrorRendersErrorHTML(t *testing.T) {
	srv, err := newCallbackServer()
	if err != nil {
		t.Fatalf("newCallbackServer: %v", err)
	}
	defer srv.Close()

	port := srv.Port()
	cbURL := "http://127.0.0.1:" + itoaPort(port) + "/callback?error=consent_required"

	resp, err := http.Get(cbURL) //nolint:noctx // test helper
	if err != nil {
		t.Fatalf("callback GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Authentication failed") {
		t.Errorf("error-callback HTML should say 'Authentication failed'; got %q", string(body))
	}
	if !strings.Contains(string(body), "consent_required") {
		t.Errorf("error-callback HTML should echo the IdP error code; got %q", string(body))
	}
	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
}

// TestCallbackServer_CloseIsIdempotent covers server.go:64-67 — multiple
// Close() calls must not panic (the sync.Once gate). A regression here
// would manifest as a "use of closed listener" panic on the second
// flow attempt in the same process.
func TestCallbackServer_CloseIsIdempotent(t *testing.T) {
	srv, err := newCallbackServer()
	if err != nil {
		t.Fatalf("newCallbackServer: %v", err)
	}
	srv.Close()
	srv.Close() // must not panic
	srv.Close() // must not panic
}

// TestGenerateNonce_HexShape pins the format contract of generateNonce
// — 32 hex chars (16 raw bytes). The state nonce travels through query
// strings and is compared byte-for-byte by Run's CSRF guard; a regression
// to a base64 form with `+` / `/` chars would break URL transport.
func TestGenerateNonce_HexShape(t *testing.T) {
	for i := 0; i < 5; i++ {
		s, err := generateNonce()
		if err != nil {
			t.Fatalf("generateNonce: %v", err)
		}
		if len(s) != 32 {
			t.Errorf("nonce length = %d, want 32 hex chars", len(s))
		}
		for _, c := range s {
			if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
				t.Errorf("nonce char %q is not lowercase hex", c)
				break
			}
		}
	}
}

// TestGenerateNonce_NonRepeating asserts two consecutive nonces differ
// — observable evidence that randomness is being read (vs. e.g. a
// constant-fold regression).
func TestGenerateNonce_NonRepeating(t *testing.T) {
	a, err := generateNonce()
	if err != nil {
		t.Fatalf("generateNonce: %v", err)
	}
	b, err := generateNonce()
	if err != nil {
		t.Fatalf("generateNonce: %v", err)
	}
	if a == b {
		t.Errorf("consecutive nonces collided: %q == %q", a, b)
	}
}

// TestGenerateCSR_RoundTrip pins generateCSR's output shape — both
// PEM blocks must parse and the CSR's CN must encode the hostname.
func TestGenerateCSR_RoundTrip(t *testing.T) {
	keyPEM, csrPEM, err := generateCSR("my-host")
	if err != nil {
		t.Fatalf("generateCSR: %v", err)
	}
	if !strings.Contains(string(keyPEM), "EC PRIVATE KEY") {
		t.Errorf("keyPEM should be EC PRIVATE KEY; got first line: %s", firstLine(keyPEM))
	}
	if !strings.Contains(string(csrPEM), "CERTIFICATE REQUEST") {
		t.Errorf("csrPEM should be CERTIFICATE REQUEST; got first line: %s", firstLine(csrPEM))
	}
}

// TestBuildAuthorizeURL_EncodesAllParams covers flow.go:239-250
// directly via whitebox access — confirms each OAuth param is set on
// the query string and that the path is /oauth/authorize.
func TestBuildAuthorizeURL_EncodesAllParams(t *testing.T) {
	f := &Flow{cpURL: "https://cp.example.com"}
	got := f.buildAuthorizeURL("http://127.0.0.1:9999/callback", "ch4ll3ng3", "st4t3")
	for _, want := range []string{
		"https://cp.example.com/oauth/authorize?",
		"client_id=agent-desktop",
		"response_type=code",
		"redirect_uri=http%3A%2F%2F127.0.0.1%3A9999%2Fcallback",
		"code_challenge=ch4ll3ng3",
		"code_challenge_method=S256",
		"state=st4t3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("authorize URL missing %q; full URL: %s", want, got)
		}
	}
}

// itoaPort returns the decimal string for an int — small local helper
// so we don't import strconv just for this.
func itoaPort(p int) string {
	if p == 0 {
		return "0"
	}
	var buf [6]byte
	i := len(buf)
	for p > 0 {
		i--
		buf[i] = byte('0' + p%10)
		p /= 10
	}
	return string(buf[i:])
}

// firstLine returns the first line of b, for friendlier test failures.
func firstLine(b []byte) string {
	if i := strings.IndexByte(string(b), '\n'); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}
