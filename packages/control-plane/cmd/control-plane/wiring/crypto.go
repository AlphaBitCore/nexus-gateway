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
func InitCrypto(cfg *config.Config, logger *slog.Logger) (CryptoResult, error) {
	vault, err := crypto.InitVault(crypto.VaultConfig{
		EncryptionKey:        cfg.Crypto.EncryptionKey,
		EncryptionPassphrase: cfg.Crypto.EncryptionPassphrase,
		EncryptionSalt:       cfg.Crypto.EncryptionSalt,
		Production:           cfg.Crypto.Production,
	}, logger)
	if err != nil {
		return CryptoResult{}, err
	}

	var multiVault *crypto.MultiVault
	if cfg.Crypto.CredentialKeyMap != "" {
		multiVault, err = crypto.NewMultiVault(cfg.Crypto.CredentialKeyMap, logger)
		if err != nil {
			return CryptoResult{}, err
		}
	}

	return CryptoResult{Vault: vault, MultiVault: multiVault}, nil
}
