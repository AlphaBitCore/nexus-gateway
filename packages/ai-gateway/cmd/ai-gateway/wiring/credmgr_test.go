package wiring

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
)

// TestInitCredManager_nilCacheLayer returns a non-nil manager. The Manager
// accepts a nil cache layer and degrades gracefully (no decryption possible).
func TestInitCredManager_nilCacheLayer(t *testing.T) {
	cfg := &config.Config{}
	mgr, err := InitCredManager(cfg, nil, nil, discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mgr == nil {
		t.Fatal("expected non-nil Manager for empty config")
	}
}

// TestInitCredManager_withMasterKey verifies a valid master key is accepted.
func TestInitCredManager_withMasterKey(t *testing.T) {
	cfg := &config.Config{}
	// 32-byte hex master key that creddecrypt.NewDecryptor accepts.
	cfg.Auth.CredentialMasterKey = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	mgr, err := InitCredManager(cfg, nil, nil, discardLogger())
	if err != nil {
		t.Fatalf("unexpected error with valid master key: %v", err)
	}
	if mgr == nil {
		t.Fatal("expected non-nil Manager")
	}
}

// TestInitCredManager_invalidMasterKey returns error for bad key format.
func TestInitCredManager_invalidMasterKey(t *testing.T) {
	cfg := &config.Config{}
	cfg.Auth.CredentialMasterKey = "not-a-hex-key!"
	_, err := InitCredManager(cfg, nil, nil, discardLogger())
	if err == nil {
		t.Fatal("expected error for invalid master key format")
	}
}

// TestInitCredManager_withKeyMap verifies multi-key decryptor path is exercised.
func TestInitCredManager_withKeyMap(t *testing.T) {
	cfg := &config.Config{}
	// Format: "keyID:hexKey" — must be a valid 32-byte hex key.
	cfg.Auth.CredentialKeyMap = "v1:0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	mgr, err := InitCredManager(cfg, nil, nil, discardLogger())
	if err != nil {
		t.Fatalf("unexpected error with valid key map: %v", err)
	}
	if mgr == nil {
		t.Fatal("expected non-nil Manager for key map")
	}
}

// TestInitMQProducer_emptyDriverReturnsNil verifies empty driver is a no-op.
func TestInitMQProducer_emptyDriverReturnsNil(t *testing.T) {
	cfg := &config.Config{}
	// Empty driver → no MQ producer (nil, nil).
	producer, err := InitMQProducer(cfg, discardLogger())
	if err != nil {
		t.Fatalf("unexpected error for empty driver: %v", err)
	}
	if producer != nil {
		t.Error("expected nil producer for empty driver")
	}
}
