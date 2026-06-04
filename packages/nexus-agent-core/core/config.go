package core

// Env is one named target. It holds only non-secret references; secrets
// (tokens, admin key, VK secret) live in the OS keychain (see SecretStore).
//
// The toml struct tags are inert inside the kernel — the on-disk profile store
// that reads/writes them lives in the CLI module (nexus-cli/internal/local,
// type Config). Env stays here because it is the transport-shaped target
// descriptor every kernel consumer (client, authenticator, chat) needs; the
// kernel deliberately carries no toml dependency.
type Env struct {
	Name             string `toml:"name"`
	CPBaseURL        string `toml:"cp_base_url"`
	AIGatewayBaseURL string `toml:"ai_gateway_base_url"`
	OAuthClientID    string `toml:"oauth_client_id"`
	OAuthRedirectURI string `toml:"oauth_redirect_uri"` // headless/registered redirect; browser flow uses a loopback URI
	IsProd           bool   `toml:"is_prod"`
	LastModel        string `toml:"last_model"`
	LastVKID         string `toml:"last_vk_id"`
	LastVKName       string `toml:"last_vk_name"`
}

// firstNonEmpty returns the first non-empty argument. Kept in the kernel because
// core/client.go uses it for IAM-action fallback; the local profile store keeps
// its own copy so the two packages stay decoupled.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
