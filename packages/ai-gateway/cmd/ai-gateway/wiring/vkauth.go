// vkauth.go — virtual key authenticator wiring.
package wiring

import (
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	cachelayer "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/layer"
)

// InitVKAuth validates the HMAC secret and constructs the VK authenticator.
func InitVKAuth(cacheLayer *cachelayer.Layer, hmacSecret string, logger *slog.Logger) (*vkauth.Authenticator, error) {
	if err := vkauth.ValidateHMACSecret(hmacSecret); err != nil {
		return nil, err
	}
	return vkauth.NewAuthenticator(cacheLayer, hmacSecret, logger), nil
}
