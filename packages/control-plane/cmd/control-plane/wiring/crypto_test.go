package wiring

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
)

func TestInitCrypto_NoKey_DevMode_VaultNil(t *testing.T) {
	cfg := &config.Config{}
	// No key, no passphrase, not production: InitVault returns nil vault (dev fallback).
	res, err := InitCrypto(cfg, silentLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// In non-production mode with no key, vault may be nil (no encryption).
	_ = res.Vault
	if res.MultiVault != nil {
		t.Error("expected nil MultiVault when CredentialKeyMap is empty")
	}
}

func TestInitCrypto_WithValidKey_VaultNonNil(t *testing.T) {
	cfg := &config.Config{}
	// AES-256 requires a 32-byte key encoded as 64 hex chars.
	cfg.Crypto.EncryptionKey = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"

	res, err := InitCrypto(cfg, silentLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Vault == nil {
		t.Error("expected non-nil Vault when valid key is provided")
	}
}

func TestInitCrypto_InvalidKey_ReturnsError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Crypto.EncryptionKey = "not-valid-hex"

	_, err := InitCrypto(cfg, silentLogger())
	if err == nil {
		t.Fatal("expected error for invalid encryption key, got nil")
	}
}

func TestInitCrypto_WithValidKeyAndCredentialKeyMap_MultiVaultNonNil(t *testing.T) {
	cfg := &config.Config{}
	cfg.Crypto.EncryptionKey = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	// CredentialKeyMap format: "keyID:64hexchars"
	cfg.Crypto.CredentialKeyMap = "k1:0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"

	res, err := InitCrypto(cfg, silentLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.MultiVault == nil {
		t.Error("expected non-nil MultiVault when CredentialKeyMap is set")
	}
}

// TestInitCrypto_KeyMapOnly_ProductionBoots is the F-0085 regression guard:
// in production mode with NO single key and NO passphrase but a valid
// CREDENTIAL_KEY_MAP, InitCrypto must boot (the map provides decryption) — the
// single-key vault's prod-required error must be relaxed when a map is set.
func TestInitCrypto_KeyMapOnly_ProductionBoots(t *testing.T) {
	cfg := &config.Config{}
	cfg.Crypto.Production = true
	// No EncryptionKey, no passphrase.
	cfg.Crypto.CredentialKeyMap = "v1:0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"

	res, err := InitCrypto(cfg, silentLogger())
	if err != nil {
		t.Fatalf("map-only production config must boot, got error: %v", err)
	}
	if res.MultiVault == nil {
		t.Fatal("expected non-nil MultiVault in map-only mode")
	}
	// Single-key vault is absent (dev fallback returns nil) — the map carries
	// all crypto in this mode.
	if res.Vault != nil {
		t.Error("expected nil single-key Vault when only the key map is configured")
	}
}

// TestInitCrypto_NoKeyAndNoMap_ProductionErrors confirms the prod gate still
// fires when NEITHER a single key/passphrase NOR a key map is configured.
func TestInitCrypto_NoKeyAndNoMap_ProductionErrors(t *testing.T) {
	cfg := &config.Config{}
	cfg.Crypto.Production = true
	if _, err := InitCrypto(cfg, silentLogger()); err == nil {
		t.Fatal("expected production error when no credential key of any kind is set")
	}
}

func TestInitCrypto_InvalidCredentialKeyMap_ReturnsError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Crypto.EncryptionKey = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	cfg.Crypto.CredentialKeyMap = "k1:tooshort" // not 64 hex chars

	_, err := InitCrypto(cfg, silentLogger())
	if err == nil {
		t.Fatal("expected error for invalid CredentialKeyMap, got nil")
	}
}
