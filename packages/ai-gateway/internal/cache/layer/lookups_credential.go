// lookups_credential.go — the credential half of the cache layer's read API.
//
// Its own file because doc lockstep maps code to docs per FILE: the
// credential-pool-and-resolution entry needs to fire when credential selection
// or the usable-credential predicate changes, and while these lived in
// lookups.go beside the provider, model, virtual-key, routing-rule and pricing
// lookups, every edit to any of those demanded an unrelated credentials-doc
// update. The map could not express "the credential half", because the file
// was five concerns wide.
package cachelayer

import (
	"context"
	"fmt"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// GetCredentialByID returns the Credential row by ID.
func (l *Layer) GetCredentialByID(ctx context.Context, id string) (*store.Credential, error) {
	if c, ok := l.credentials.Get(id); ok {
		v := c
		return &v, nil
	}
	return nil, fmt.Errorf("cachelayer: credential %q: %w", id, errNotFound)
}

// GetCredentialForProvider returns the first enabled, active credential for a
// provider by consulting the precomputed secondary index.
func (l *Layer) GetCredentialForProvider(ctx context.Context, providerID string) (*store.Credential, error) {
	idx := l.credentialsByProviderFirst.Load()
	if idx == nil {
		return nil, fmt.Errorf("cachelayer: credential for provider %q: %w", providerID, ErrIndexUnavailable)
	}
	if c, ok := (*idx)[providerID]; ok {
		v := c
		return &v, nil
	}
	return nil, fmt.Errorf("cachelayer: credential for provider %q: %w", providerID, errNotFound)
}

// ListCredentialsForProvider returns all enabled, active credentials for a
// provider from the snapshot. Used by the multi-credential pool selector.
func (l *Layer) ListCredentialsForProvider(ctx context.Context, providerID string) ([]store.Credential, error) {
	all := l.credentials.All()
	var out []store.Credential
	for _, c := range all {
		if c.ProviderID == providerID && c.Enabled && c.Status == "active" && c.SelectionWeight > 0 {
			out = append(out, c)
		}
	}
	return out, nil
}

// CredentialsAll returns the full Credential snapshot for runtime introspection (e31-s7).
// CALLERS MUST REDACT EncryptedKey / EncryptionIv / EncryptionTag before exposing
// over a public surface. Provided here as an unredacted internal accessor —
// consumers (introspection wiring) layer the redaction on top.
func (l *Layer) CredentialsAll() map[string]store.Credential {
	if l == nil || l.credentials == nil {
		return nil
	}
	return l.credentials.All()
}
