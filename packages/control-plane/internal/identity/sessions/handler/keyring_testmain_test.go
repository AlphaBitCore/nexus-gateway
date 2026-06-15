package me

import (
	"os"
	"testing"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/hmackeyring"
)

// TestMain installs a fixed single-version HMAC keyring so this package's API-key
// hashing (auth.HashAPIKey) has an injected keyring — mirroring the boot-time
// auth.InitHMACKeyring (SEC-W2-01 Layer A). Production always injects at boot;
// tests must too, since the hashing layer no longer falls back to an empty
// secret (a nil keyring is a wiring bug, not a silent default).
func TestMain(m *testing.M) {
	kr, err := hmackeyring.Single("test-hmac-secret")
	if err != nil {
		panic(err)
	}
	if err := auth.InitHMACKeyring(kr); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
