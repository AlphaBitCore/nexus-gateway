package auth

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/hmackeyring"
)

// SEC-W2-01 Layer A: InitHMACKeyring fails closed on a nil keyring — the config
// layer always builds a non-nil keyring or fails boot, so nil here is a wiring
// bug that must abort rather than hash keys under an empty keyring.
func TestInitHMACKeyring_NilFailsClosed(t *testing.T) {
	if err := InitHMACKeyring(nil); err == nil {
		t.Error("expected failure with a nil keyring (must fail closed, dev or prod)")
	}
}

// InitHMACKeyring installs a non-nil keyring and the hashing layer then keys
// under its current version — NOT os.Getenv at point-of-use, which is what lets
// the operator KMS-wrap + rotate the secret.
func TestInitHMACKeyring_SetInjects(t *testing.T) {
	restore := injectedKeyring
	t.Cleanup(func() { injectedKeyring = restore })

	kr, err := hmackeyring.Single("real-secret")
	if err != nil {
		t.Fatalf("Single: %v", err)
	}
	if err := InitHMACKeyring(kr); err != nil {
		t.Fatalf("InitHMACKeyring should pass with a keyring set: %v", err)
	}
	if injectedKeyring == nil || injectedKeyring.CurrentVersion() != "v1" {
		t.Errorf("injectedKeyring not installed at v1; got %v", injectedKeyring)
	}
}

// TestHashAPIKeyVersions_CurrentFirstMatchesHashAPIKey: the try-all admission
// hashes lead with the current version, and that current-version hash equals
// HashAPIKey(key) (the hash stamped at issue), so a freshly issued key always
// hits on the FIRST admission probe.
func TestHashAPIKeyVersions_CurrentFirstMatchesHashAPIKey(t *testing.T) {
	restore := injectedKeyring
	t.Cleanup(func() { injectedKeyring = restore })

	kr, err := hmackeyring.New("v1:old-secret,*v2:current-secret")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := InitHMACKeyring(kr); err != nil {
		t.Fatalf("InitHMACKeyring: %v", err)
	}

	const key = "nxk_deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	versions := HashAPIKeyVersions(key)
	if len(versions) != 2 {
		t.Fatalf("HashAPIKeyVersions len = %d, want 2", len(versions))
	}
	if versions[0].Version != "v2" {
		t.Errorf("first probe version = %q, want current v2", versions[0].Version)
	}
	if versions[0].Hash != HashAPIKey(key) {
		t.Error("current-version probe hash != HashAPIKey(key); a freshly issued key would miss the first probe")
	}
	// The older version yields a DIFFERENT hash (distinct secret) — that's what
	// makes a rotated-away key still resolvable but under a non-current version.
	if versions[1].Version != "v1" || versions[1].Hash == versions[0].Hash {
		t.Errorf("second probe = %+v; want v1 with a distinct hash", versions[1])
	}
	if CurrentKeyVersion() != "v2" {
		t.Errorf("CurrentKeyVersion = %q, want v2", CurrentKeyVersion())
	}
}
