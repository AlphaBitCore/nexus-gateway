package auth

import (
	"os"
	"testing"
)

func TestValidateHMACSecretDev(t *testing.T) {
	_ = os.Unsetenv("ADMIN_KEY_HMAC_SECRET")
	_ = os.Unsetenv("NODE_ENV")

	if err := ValidateHMACSecret(); err != nil {
		t.Errorf("should pass in dev: %v", err)
	}
}

func TestValidateHMACSecretProdMissing(t *testing.T) {
	_ = os.Unsetenv("ADMIN_KEY_HMAC_SECRET")
	_ = os.Setenv("NODE_ENV", "production")
	defer os.Unsetenv("NODE_ENV") //nolint:errcheck

	if err := ValidateHMACSecret(); err == nil {
		t.Error("should fail in production without secret")
	}
}

func TestValidateHMACSecretProdSet(t *testing.T) {
	_ = os.Setenv("ADMIN_KEY_HMAC_SECRET", "real-secret")
	_ = os.Setenv("NODE_ENV", "production")
	defer os.Unsetenv("ADMIN_KEY_HMAC_SECRET") //nolint:errcheck
	defer os.Unsetenv("NODE_ENV")              //nolint:errcheck

	if err := ValidateHMACSecret(); err != nil {
		t.Errorf("should pass with secret set: %v", err)
	}
}
