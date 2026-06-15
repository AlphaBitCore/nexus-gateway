package wiring

import (
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/crypto"
)

// CryptoResult holds the single-key vault and the optional multi-key vault.
type CryptoResult struct {
	Vault      *crypto.Vault
	MultiVault *crypto.MultiVault
}

// InitCrypto initialises the AES-256-GCM credential vault and, when
// cfg.Crypto.CredentialKeyMap is non-empty, the multi-key vault that takes
// precedence over the single-key vault for encryption operations.
//
// CREDENTIAL_KEY_MAP is a standalone mode: when it is set, the
// multi-key vault alone provides encryption/decryption, so the single-key
// vault's production-required check is relaxed — InitVault's "key required in
// production" error is treated as "no single vault" rather than a fatal boot
// failure. A map-only deployment is therefore bootable, matching the
// .env.example documentation that CREDENTIAL_KEY_MAP "takes precedence when
// present". The map is built first so a malformed map still fails loudly.
func InitCrypto(cfg *config.Config, logger *slog.Logger) (CryptoResult, error) {
	var multiVault *crypto.MultiVault
	if cfg.Crypto.CredentialKeyMap != "" {
		mv, err := crypto.NewMultiVault(cfg.Crypto.CredentialKeyMap, logger)
		if err != nil {
			return CryptoResult{}, err
		}
		multiVault = mv
	}

	vault, err := crypto.InitVault(crypto.VaultConfig{
		EncryptionKey: cfg.Crypto.EncryptionKey,
		// When a key map is configured it satisfies the production
		// credential-key requirement on its own, so do not force the
		// single-key vault's prod gate.
		Production: cfg.Crypto.Production && multiVault == nil,
	}, logger)
	if err != nil {
		return CryptoResult{}, err
	}

	return CryptoResult{Vault: vault, MultiVault: multiVault}, nil
}
