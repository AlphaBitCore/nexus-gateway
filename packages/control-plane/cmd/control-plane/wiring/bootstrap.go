package wiring

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/hmackeyring"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/logging"
)

// Bootstrap holds the outputs of the early boot phase.
type Bootstrap struct {
	Config    *config.Config
	Logger    *slog.Logger
	StartTime time.Time
}

// InitBootstrap loads config, initializes the logger, and validates the HMAC
// secret and auth-server issuer. Returns an error for any hard failure; the
// caller should log it and exit.
func InitBootstrap(configPath string) (Bootstrap, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return Bootstrap{}, fmt.Errorf("load config: %w", err)
	}

	logger, err := logging.NewLogger(logging.Config{
		Level:        cfg.Log.Level,
		Format:       cfg.Log.Format,
		File:         cfg.Log.File,
		StackOnError: cfg.Log.StackOnError,
	})
	if err != nil {
		return Bootstrap{}, fmt.Errorf("initialize logger: %w", err)
	}
	slog.SetDefault(logger)

	// Build the versioned HMAC keyring and install it into the apikey hashing
	// layer. config.Load resolved both
	// ADMIN_KEY_HMAC_KEY_MAP and ADMIN_KEY_HMAC_SECRET through the SecretCustody
	// loader, so under provider "command" these hold the UNWRAPPED plaintext (not
	// the env-delivered wrapped blob). The keymap (versioned, rotatable) takes
	// precedence; otherwise the single secret is a one-version (v1) keyring.
	// config.validate() already guaranteed at least one is non-empty. Injected
	// here — before any handler is wired or any request authenticates.
	keyring, err := buildHMACKeyring(cfg)
	if err != nil {
		return Bootstrap{}, fmt.Errorf("build HMAC keyring: %w", err)
	}
	if err := auth.InitHMACKeyring(keyring); err != nil {
		return Bootstrap{}, fmt.Errorf("HMAC keyring init: %w", err)
	}
	// Operator visibility for rotation runbooks: version ids ONLY, never
	// secret bytes. "key map overrides" flags that ADMIN_KEY_HMAC_KEY_MAP is
	// in effect and any ADMIN_KEY_HMAC_SECRET value is ignored.
	logger.Info("HMAC keyring initialized",
		"versions", keyring.Versions(),
		"current", keyring.CurrentVersion(),
		"keyMapOverridesSingleSecret", cfg.Auth.HMACKeyMap != "",
	)
	if cfg.AuthServer.Issuer == "" {
		return Bootstrap{}, fmt.Errorf("authServer.issuer is required; set it in config YAML or AUTH_SERVER_ISSUER env")
	}

	return Bootstrap{Config: cfg, Logger: logger, StartTime: time.Now().UTC()}, nil
}

// buildHMACKeyring constructs the versioned HMAC keyring from the
// custody-resolved config. The versioned ADMIN_KEY_HMAC_KEY_MAP (rotatable) takes
// precedence; otherwise the single ADMIN_KEY_HMAC_SECRET becomes a one-version
// (v1) keyring. config.validate() guarantees at least one is non-empty.
func buildHMACKeyring(cfg *config.Config) (*hmackeyring.Keyring, error) {
	if cfg.Auth.HMACKeyMap != "" {
		return hmackeyring.New(cfg.Auth.HMACKeyMap)
	}
	return hmackeyring.Single(cfg.Auth.HMACSecret)
}
