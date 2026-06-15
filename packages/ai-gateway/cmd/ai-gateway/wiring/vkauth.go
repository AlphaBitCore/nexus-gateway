// vkauth.go — virtual key authenticator wiring.
package wiring

import (
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	cachelayer "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/layer"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/hmackeyring"
)

// InitVKAuth constructs the VK authenticator from the HMAC keyring.
// config.Load resolved both ADMIN_KEY_HMAC_KEY_MAP and
// ADMIN_KEY_HMAC_SECRET through the SecretCustody loader (so under provider
// "command" these hold the UNWRAPPED plaintext). The keymap (versioned,
// rotatable) takes precedence; otherwise the single secret is a one-version (v1)
// keyring. config.validate() already guaranteed at least one is non-empty.
func InitVKAuth(cacheLayer *cachelayer.Layer, cfg *config.Config, logger *slog.Logger) (*vkauth.Authenticator, error) {
	keyring, err := buildHMACKeyring(cfg)
	if err != nil {
		return nil, err
	}
	// Operator visibility for rotation runbooks: version ids ONLY, never
	// secret bytes. "key map overrides" flags that ADMIN_KEY_HMAC_KEY_MAP is
	// in effect and any ADMIN_KEY_HMAC_SECRET value is ignored.
	logger.Info("HMAC keyring initialized",
		"versions", keyring.Versions(),
		"current", keyring.CurrentVersion(),
		"keyMapOverridesSingleSecret", cfg.Auth.HMACKeyMap != "",
	)
	return vkauth.NewAuthenticator(cacheLayer, keyring, logger), nil
}

// buildHMACKeyring constructs the versioned HMAC keyring from the
// custody-resolved config — the keymap (rotatable) takes precedence, else the
// single secret becomes a one-version (v1) keyring. [MUST MATCH] the Control
// Plane's bootstrap.buildHMACKeyring so both derive identical per-version VK
// hashes.
func buildHMACKeyring(cfg *config.Config) (*hmackeyring.Keyring, error) {
	if cfg.Auth.HMACKeyMap != "" {
		return hmackeyring.New(cfg.Auth.HMACKeyMap)
	}
	return hmackeyring.Single(cfg.Auth.HMACSecret)
}
