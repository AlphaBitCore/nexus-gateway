package cachelayer

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// errNotFound is returned for cache lookups that miss the snapshot.
// Wrapped with pgx.ErrNoRows so existing callers that switch on
// errors.Is(err, pgx.ErrNoRows) continue to behave correctly.
var errNotFound = pgx.ErrNoRows

// GetProvider returns the Provider row by ID.
func (l *Layer) GetProvider(ctx context.Context, id string) (*store.Provider, error) {
	if p, ok := l.providers.Get(id); ok {
		// Return a copy to prevent callers from mutating the snapshot.
		v := p
		return &v, nil
	}
	return nil, fmt.Errorf("cachelayer: provider %q: %w", id, errNotFound)
}

// GetModel returns the Model row by ID.
func (l *Layer) GetModel(ctx context.Context, id string) (*store.Model, error) {
	if m, ok := l.models.Get(id); ok {
		v := m
		return &v, nil
	}
	return nil, fmt.Errorf("cachelayer: model %q: %w", id, errNotFound)
}

// GetModelByCode returns the enabled Model row matching a customer-facing
// code. Mirrors store.DB.GetModelByCode semantics.
func (l *Layer) GetModelByCode(ctx context.Context, code string) (*store.Model, error) {
	idx := l.modelsByCode.Load()
	if idx != nil {
		if m, ok := (*idx)[code]; ok {
			v := m
			return &v, nil
		}
	}
	return nil, fmt.Errorf("cachelayer: model code %q: %w", code, errNotFound)
}

// GetModelByCodeOrAlias returns the enabled Model row whose customer-facing
// code OR one of its aliases matches key, resolved in O(1) from the in-memory
// code-or-alias index (no per-request scan, no DB read). Codes take priority
// over aliases. Used by the routing passthrough fallback so an aliased model
// routes without an explicit routing rule; GetModelByCode stays strict-code.
func (l *Layer) GetModelByCodeOrAlias(ctx context.Context, key string) (*store.Model, error) {
	idx := l.modelsByCodeOrAlias.Load()
	if idx != nil {
		if m, ok := (*idx)[key]; ok {
			v := m
			return &v, nil
		}
	}
	return nil, fmt.Errorf("cachelayer: model code/alias %q: %w", key, errNotFound)
}

// AllModels returns a copy of every Model row in the current snapshot.
// Used by the capability cache rebuild hook in configdispatch after a
// models reload so the pre-filter stays in sync without a second DB query.
func (l *Layer) AllModels() []store.Model {
	raw := l.models.All()
	out := make([]store.Model, 0, len(raw))
	for _, m := range raw {
		out = append(out, m)
	}
	return out
}

// ResolveModelCandidates returns every enabled Model whose `code`
// equals the request string OR whose `aliases` contains it. Walks the
// Model snapshot in-memory; the catalog is small and bounded, so the
// linear scan is cheaper than maintaining another index. Mirrors
// store.DB.ResolveModelCandidates so router.routingStore can swap
// freely between the two.
func (l *Layer) ResolveModelCandidates(ctx context.Context, code string) ([]store.Model, error) {
	if code == "" {
		return nil, nil
	}
	all := l.models.All()
	var out []store.Model
	for _, m := range all {
		if !m.Enabled {
			continue
		}
		if m.Code == code {
			out = append(out, m)
			continue
		}
		for _, a := range m.Aliases {
			if a == code {
				out = append(out, m)
				break
			}
		}
	}
	return out, nil
}

// ListEnabledModels returns a deterministic ordered slice of enabled
// models (provider then name), matching store.DB.ListEnabledModels.
func (l *Layer) ListEnabledModels(ctx context.Context) ([]store.Model, error) {
	all := l.models.All()
	out := make([]store.Model, 0, len(all))
	for _, m := range all {
		if m.Enabled {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProviderID != out[j].ProviderID {
			return out[i].ProviderID < out[j].ProviderID
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

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
	if idx != nil {
		if c, ok := (*idx)[providerID]; ok {
			v := c
			return &v, nil
		}
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

// GetVirtualKeyByHash looks up a VK by its HMAC hash. Cache miss falls
// through to the database.
func (l *Layer) GetVirtualKeyByHash(ctx context.Context, hash string) (*store.VirtualKey, error) {
	vk, err := l.vkeys.Get(ctx, hash)
	if err != nil {
		return nil, err
	}
	return vk, nil
}

// GetProviderAndModel reads both rows from the snapshots in a single
// call. Mirrors store.DB.GetProviderAndModel.
func (l *Layer) GetProviderAndModel(ctx context.Context, providerID, modelID string) (*store.Provider, *store.Model, error) {
	p, err := l.GetProvider(ctx, providerID)
	if err != nil {
		return nil, nil, err
	}
	m, err := l.GetModel(ctx, modelID)
	if err != nil {
		return nil, nil, err
	}
	return p, m, nil
}

// GetEnabledRoutingRules delegates to the underlying *store.DB whose
// rulesCache (per-DB instance, 30-min TTL, singleflight) is the
// canonical routing-rules cache. Cachelayer does not yet hold its own
// snapshot for routing rules — migration to SnapshotCache is tracked
// as a follow-up. Wiring this method here lets cachelayer.Layer
// satisfy router.routingStore so callers don't need a second handle.
// ProvidersAll returns the full Provider snapshot for runtime introspection (e31-s7).
// No secrets in Provider — caller may pass-through.
func (l *Layer) ProvidersAll() map[string]store.Provider {
	if l == nil || l.providers == nil {
		return nil
	}
	return l.providers.All()
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

func (l *Layer) GetEnabledRoutingRules(ctx context.Context) ([]store.RoutingRule, error) {
	return l.db.GetEnabledRoutingRules(ctx)
}

// InvalidateRoutingRules forwards to the bespoke rulesCache.
func (l *Layer) InvalidateRoutingRules() {
	l.db.InvalidateRuleCache()
}

// FetchModelPricing returns pricing rows for the requested model IDs by
// reading the Model snapshot. Mirrors store.DB.FetchModelPricing return
// shape — including the empty-pricing zero row for IDs missing from the
// snapshot, so the quota downgrade chain matches its previous semantics.
func (l *Layer) FetchModelPricing(ctx context.Context, modelIDs []string) ([]store.ModelPricing, error) {
	if len(modelIDs) == 0 {
		return nil, nil
	}
	out := make([]store.ModelPricing, len(modelIDs))
	for i, id := range modelIDs {
		mp := store.ModelPricing{ModelID: id}
		if m, ok := l.models.Get(id); ok {
			if m.InputPricePM != nil {
				mp.InputPricePM = *m.InputPricePM
			}
			if m.OutputPricePM != nil {
				mp.OutputPricePM = *m.OutputPricePM
			}
		}
		out[i] = mp
	}
	return out, nil
}

// IsNotFound reports whether err signals a missing row from any of the
// Get* lookups above. Mirrors errors.Is(err, pgx.ErrNoRows).
func IsNotFound(err error) bool {
	return errors.Is(err, errNotFound)
}
