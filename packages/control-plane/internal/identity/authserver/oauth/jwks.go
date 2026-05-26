// Package oauth holds HTTP handlers for the control plane's OAuth/OIDC
// endpoints. Each file in this package implements one spec-defined endpoint.
package oauth

import (
	"encoding/base64"
	"math/big"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
)

type jwk struct {
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// JWKSHandler returns an Echo handler that serves the JSON Web Key Set for the
// given keystore per RFC 7517. Public keys are encoded as base64url big-endian
// unsigned integers, with RS256 as the advertised algorithm.
func JWKSHandler(ks *token.Keystore) echo.HandlerFunc {
	return func(c echo.Context) error {
		keys := ks.All()
		out := struct {
			Keys []jwk `json:"keys"`
		}{Keys: make([]jwk, 0, len(keys))}
		for _, k := range keys {
			pub := k.Priv.PublicKey
			out.Keys = append(out.Keys, jwk{
				Kty: "RSA", Alg: "RS256", Use: "sig", Kid: k.KID,
				N: base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			})
		}
		c.Response().Header().Set("Cache-Control", "public, max-age=300")
		return c.JSON(http.StatusOK, out)
	}
}
