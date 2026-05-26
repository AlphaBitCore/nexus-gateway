package token

// AdminAudience is the single fixed `aud` claim value that all access tokens
// minted by the CP auth server carry and that all CP admin resource verifiers
// enforce. Every JWT consumer in Nexus Gateway must reference this constant
// rather than a literal — drift between mint and verify silently invalidates
// every token.
//
// Home rationale: the natural home is the authserver root package, but
// authserver imports authserver/oauth (via mount.go) and authserver/oauth
// is one of the two call sites, so placing the constant at the root would
// create an import cycle. authserver/token is the deepest package that both
// the CP verifier wiring (cmd/control-plane/main.go) and the CP minter
// (authserver/oauth/token.go) already import, so it is the cycle-free single
// source of truth.
const AdminAudience = "cp-admin"
