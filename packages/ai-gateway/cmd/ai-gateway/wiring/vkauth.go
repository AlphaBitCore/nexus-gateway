// vkauth.go — virtual key authenticator wiring.
package wiring

import (
	"log/slog"

	cachelayer "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/layer"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
)

// InitVKAuth validates the HMAC secret and constructs the VK authenticator.
func InitVKAuth(cacheLayer *cachelayer.Layer, hmacSecret string, logger *slog.Logger) (*vkauth.Authenticator, error) {
	if err := vkauth.ValidateHMACSecret(hmacSecret); err != nil {
		return nil, err
	}
	return vkauth.NewAuthenticator(cacheLayer, hmacSecret, logger), nil
}
