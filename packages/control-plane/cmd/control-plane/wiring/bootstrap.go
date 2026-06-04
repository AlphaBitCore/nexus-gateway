package wiring

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
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
	// Bridge YAML auth.hmacSecret into env before ValidateHMACSecret runs.
	if cfg.Auth.HMACSecret != "" && os.Getenv("ADMIN_KEY_HMAC_SECRET") == "" {
		if err := os.Setenv("ADMIN_KEY_HMAC_SECRET", cfg.Auth.HMACSecret); err != nil {
			return Bootstrap{}, fmt.Errorf("apply auth.hmacSecret: %w", err)
		}
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

	if err := auth.ValidateHMACSecret(); err != nil {
		return Bootstrap{}, fmt.Errorf("HMAC secret validation: %w", err)
	}
	if cfg.AuthServer.Issuer == "" {
		return Bootstrap{}, fmt.Errorf("authServer.issuer is required; set it in config YAML or AUTH_SERVER_ISSUER env")
	}

	return Bootstrap{Config: cfg, Logger: logger, StartTime: time.Now().UTC()}, nil
}
