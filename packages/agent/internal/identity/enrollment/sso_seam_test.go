package enrollment

// seam_test.go drives the randReader / marshalECPrivKey / netListen /
// runtimeGOOS / execCommandStart seams added to flow.go + server.go to
// reach ≥95% statement coverage on the package. The seams mirror the
// established pattern in packages/agent/internal/identity/enrollment/enroll.go
// (randReader) and packages/agent/internal/network/tls/engine.go
// (tlsRandReader) — see flow.go's package-var comment block.
//
// Each test restores the seam in t.Cleanup so a failure mid-test cannot
// leak into a sibling test (race-detector-safe + deterministic order).

import (
	"crypto/ecdsa"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
)

// ssoFailReader returns the configured error on every Read call. Used to
// simulate crypto/rand exhaustion — a state the real /dev/urandom cannot
// reach on supported OSes but every error branch downstream assumes can
// fire and must be tested through the seam.
type ssoFailReader struct{ err error }

func (f ssoFailReader) Read(_ []byte) (int, error) { return 0, f.err }

// TestGenerateNonce_RandReaderError covers flow.go:328-330 — when the
// entropy source fails, generateNonce must surface the error rather than
// return a deterministic constant. A regression here would silently
// downgrade CSRF protection.
func TestGenerateNonce_RandReaderError(t *testing.T) {
	want := errors.New("entropy exhausted")
	orig := ssoRandReader
	ssoRandReader = ssoFailReader{err: want}
	t.Cleanup(func() { ssoRandReader = orig })

	s, err := generateNonce()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want wrap of %v", err, want)
	}
	if s != "" {
		t.Errorf("nonce should be empty on entropy error, got %q", s)
	}
}

// TestGenerateSSODeviceIdentity_EcdsaGenerateKeyError — when
// ecdsa.GenerateKey fails (entropy starved) the function must surface
// "generate key:" rather than return a partially-populated PEM block.
func TestGenerateSSODeviceIdentity_EcdsaGenerateKeyError(t *testing.T) {
	want := errors.New("ecdsa: no entropy")
	orig := ssoRandReader
	ssoRandReader = ssoFailReader{err: want}
	t.Cleanup(func() { ssoRandReader = orig })

	keyPEM, certPEM, err := generateSSODeviceIdentity("h")
	if err == nil {
		t.Fatal("expected error when ecdsa.GenerateKey fails")
	}
	if !strings.Contains(err.Error(), "generate key") {
		t.Errorf("err %q should mention 'generate key'", err.Error())
	}
	if !errors.Is(err, want) {
		t.Errorf("err %q should wrap sentinel", err.Error())
	}
	if keyPEM != nil || certPEM != nil {
		t.Errorf("PEM outputs should be nil on entropy error; got keyPEM=%d certPEM=%d", len(keyPEM), len(certPEM))
	}
}

// failAfterNReader returns n successful reads then a sentinel error. Used
// to let ecdsa.GenerateKey succeed (which consumes entropy) and then make
// x509.CreateCertificateRequest fail on the next read.
type failAfterNReader struct {
	src  io.Reader
	left int
	err  error
}

func (f *failAfterNReader) Read(p []byte) (int, error) {
	if f.left <= 0 {
		return 0, f.err
	}
	if len(p) > f.left {
		p = p[:f.left]
	}
	n, err := f.src.Read(p)
	f.left -= n
	if err != nil {
		return n, err
	}
	return n, nil
}

// TestGenerateSSODeviceIdentity_CreateCertError — after the keypair and
// serial are minted but before the self-signed cert is created, an entropy
// hiccup during signing must surface as "create device cert:" not a panic.
func TestGenerateSSODeviceIdentity_CreateCertError(t *testing.T) {
	want := errors.New("entropy gone mid-cert")
	// ecdsa.GenerateKey for P-256 reads ~33 bytes; rand.Int for the 128-bit
	// serial reads ~16; x509.CreateCertificate then needs another ~32 for
	// the ECDSA signature nonce. Budget 60 bytes — enough for keygen +
	// serial, NOT enough for signing — so CreateCertificate hits the
	// sentinel and exercises the "create device cert" error arm.
	orig := ssoRandReader
	ssoRandReader = &failAfterNReader{src: rand.Reader, left: 60, err: want}
	t.Cleanup(func() { ssoRandReader = orig })

	keyPEM, certPEM, err := generateSSODeviceIdentity("h")
	if err == nil {
		t.Fatal("expected error when cert creation entropy fails")
	}
	if !strings.Contains(err.Error(), "create device cert") {
		t.Errorf("err %q should mention 'create device cert'", err.Error())
	}
	if !errors.Is(err, want) {
		t.Errorf("err %q should wrap sentinel", err.Error())
	}
	if keyPEM != nil || certPEM != nil {
		t.Errorf("PEM outputs should be nil; got keyPEM=%d certPEM=%d", len(keyPEM), len(certPEM))
	}
}

// TestGenerateSSODeviceIdentity_MarshalECPrivKeyError — when
// x509.MarshalECPrivateKey fails (corrupted key fields after the cert was
// signed), the function must surface "marshal key:" instead of writing an
// unparseable PEM block to disk on the next step. Production never fails
// here on a valid P-256 key; the seam exists so the error arm is pinned
// for the day someone swaps the curve.
func TestGenerateSSODeviceIdentity_MarshalECPrivKeyError(t *testing.T) {
	want := errors.New("marshal: corrupted curve params")
	orig := ssoMarshalECPrivKey
	ssoMarshalECPrivKey = func(_ *ecdsa.PrivateKey) ([]byte, error) { return nil, want }
	t.Cleanup(func() { ssoMarshalECPrivKey = orig })

	keyPEM, certPEM, err := generateSSODeviceIdentity("h")
	if err == nil {
		t.Fatal("expected error when MarshalECPrivateKey fails")
	}
	if !strings.Contains(err.Error(), "marshal key") {
		t.Errorf("err %q should mention 'marshal key'", err.Error())
	}
	if !errors.Is(err, want) {
		t.Errorf("err %q should wrap sentinel", err.Error())
	}
	// The cert may have been built before the marshal fault — but
	// generateSSODeviceIdentity must NOT return it because the matching
	// key is undefined.
	if keyPEM != nil || certPEM != nil {
		t.Errorf("PEM outputs should be nil on marshal error; got keyPEM=%d certPEM=%d", len(keyPEM), len(certPEM))
	}
}

// TestNewCallbackServer_ListenError covers server.go:29-31 — when
// net.Listen on 127.0.0.1:0 fails (no loopback interface, file-descriptor
// exhaustion, sandbox restriction), the constructor must surface
// "ssoenroll: listen:" so the SSO flow fails fast rather than dropping
// into a no-server state where Wait would block forever.
func TestNewCallbackServer_ListenError(t *testing.T) {
	want := errors.New("listen: too many open files")
	orig := ssoNetListen
	ssoNetListen = func(_, _ string) (net.Listener, error) { return nil, want }
	t.Cleanup(func() { ssoNetListen = orig })

	srv, err := newCallbackServer()
	if err == nil {
		if srv != nil {
			srv.Close()
		}
		t.Fatal("expected error when net.Listen fails")
	}
	if srv != nil {
		t.Error("server must be nil on listen error")
	}
	if !strings.Contains(err.Error(), "ssoenroll: listen") {
		t.Errorf("err %q should mention 'ssoenroll: listen'", err.Error())
	}
	if !errors.Is(err, want) {
		t.Errorf("err %q should wrap sentinel", err.Error())
	}
}

// TestOpenBrowser_LinuxBranch covers flow.go:341-343 — the linux arm of
// the runtime.GOOS switch. Exec is intercepted by the seam so the test
// runs on any host. Asserts the linux command + args contract: xdg-open
// with the URL as the sole argument.
func TestOpenBrowser_LinuxBranch(t *testing.T) {
	origOS, origExec := ssoRuntimeGOOS, ssoExecCommandStart
	ssoRuntimeGOOS = "linux"
	var gotCmd string
	var gotArgs []string
	ssoExecCommandStart = func(name string, args ...string) error {
		gotCmd = name
		gotArgs = append([]string(nil), args...)
		return nil
	}
	t.Cleanup(func() { ssoRuntimeGOOS, ssoExecCommandStart = origOS, origExec })

	if err := openBrowser("https://example.com/a?b=c"); err != nil {
		t.Fatalf("openBrowser linux: %v", err)
	}
	if gotCmd != "xdg-open" {
		t.Errorf("cmd = %q, want xdg-open", gotCmd)
	}
	if len(gotArgs) != 1 || gotArgs[0] != "https://example.com/a?b=c" {
		t.Errorf("args = %v, want [URL]", gotArgs)
	}
}

// TestOpenBrowser_WindowsBranch covers flow.go:344-346 — the windows arm
// of the runtime.GOOS switch. Asserts the rundll32 + FileProtocolHandler
// contract so a future refactor to e.g. `start` would be caught.
func TestOpenBrowser_WindowsBranch(t *testing.T) {
	origOS, origExec := ssoRuntimeGOOS, ssoExecCommandStart
	ssoRuntimeGOOS = "windows"
	var gotCmd string
	var gotArgs []string
	ssoExecCommandStart = func(name string, args ...string) error {
		gotCmd = name
		gotArgs = append([]string(nil), args...)
		return nil
	}
	t.Cleanup(func() { ssoRuntimeGOOS, ssoExecCommandStart = origOS, origExec })

	if err := openBrowser("https://example.com/x"); err != nil {
		t.Fatalf("openBrowser windows: %v", err)
	}
	if gotCmd != "rundll32" {
		t.Errorf("cmd = %q, want rundll32", gotCmd)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "url.dll,FileProtocolHandler" || gotArgs[1] != "https://example.com/x" {
		t.Errorf("args = %v, want [url.dll,FileProtocolHandler URL]", gotArgs)
	}
}

// TestOpenBrowser_UnsupportedOS covers flow.go:347-348 — when running on
// an OS we have not wired (freebsd, openbsd, ios, android, js), the
// function must return an "unsupported OS" error WITHOUT shelling out, so
// the SSO flow continues with the manual-paste fallback rather than
// crashing.
func TestOpenBrowser_UnsupportedOS(t *testing.T) {
	origOS, origExec := ssoRuntimeGOOS, ssoExecCommandStart
	ssoRuntimeGOOS = "plan9"
	var execCalled bool
	ssoExecCommandStart = func(string, ...string) error {
		execCalled = true
		return nil
	}
	t.Cleanup(func() { ssoRuntimeGOOS, ssoExecCommandStart = origOS, origExec })

	err := openBrowser("https://example.com/")
	if err == nil {
		t.Fatal("expected unsupported-OS error")
	}
	if !strings.Contains(err.Error(), "unsupported OS") {
		t.Errorf("err %q should mention 'unsupported OS'", err.Error())
	}
	if !strings.Contains(err.Error(), "plan9") {
		t.Errorf("err %q should echo the unsupported GOOS so an operator can read it", err.Error())
	}
	if execCalled {
		t.Error("exec must NOT be invoked on unsupported OS")
	}
}

// TestOpenBrowser_ExecStartError covers flow.go:350 — when the underlying
// exec.Command.Start fails (the open/xdg-open/rundll32 binary missing,
// fork failure), openBrowser must surface that error. The Run flow's
// `if err := openFn(...); err != nil { slog.Warn(...) }` arm depends on
// this propagation to log "could not open browser automatically" and
// continue.
func TestOpenBrowser_ExecStartError(t *testing.T) {
	origOS, origExec := ssoRuntimeGOOS, ssoExecCommandStart
	ssoRuntimeGOOS = "darwin"
	sentinel := errors.New("fork failed")
	ssoExecCommandStart = func(string, ...string) error { return sentinel }
	t.Cleanup(func() { ssoRuntimeGOOS, ssoExecCommandStart = origOS, origExec })

	err := openBrowser("https://example.com/")
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want wrap of %v", err, sentinel)
	}
}
