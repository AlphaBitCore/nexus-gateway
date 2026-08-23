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

// ErrIndexUnavailable is returned when a secondary index has not been built
// yet — the atomic pointer is still nil because no load has succeeded. It is
// deliberately NOT wrapped with errNotFound, because the two facts are
// opposite: a miss means the row does not exist and the caller should stop
// asking, while an unbuilt index means we cannot answer the question at all
// and the caller should try again.
//
// Collapsing them is not hypothetical. On 2026-08-11, six catalogued and
// enabled models — claude-sonnet-4-6, claude-opus-4-6, claude-haiku-4-5,
// gpt-4o, text-embedding-3-large, gemini-embedding-001 — returned
// "no available provider for model X / Ensure the model exists and is
// enabled" for 34 minutes on staging. Every one of them existed and was
// enabled the whole time; the index simply had not loaded, and the 404 told
// every client a permanent lie about a transient condition.
var ErrIndexUnavailable = errors.New("cachelayer: index not loaded")

// IsIndexUnavailable reports whether err signals an unbuilt index rather than
// a genuine miss. Callers that translate a lookup failure into an HTTP status
// MUST branch on this: a miss is the caller's fault (404), an unbuilt index is
// ours (503, and retryable).
func IsIndexUnavailable(err error) bool {
	return errors.Is(err, ErrIndexUnavailable)
}

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

// GetModelByCode returns the servable Model row matching a customer-facing
// code — enabled model on an enabled provider. Mirrors store.DB.GetModelByCode
// semantics.
func (l *Layer) GetModelByCode(ctx context.Context, code string) (*store.Model, error) {
	idx := l.modelsByCode.Load()
	if idx == nil {
		return nil, fmt.Errorf("cachelayer: model code %q: %w", code, ErrIndexUnavailable)
	}
	if m, ok := (*idx)[code]; ok {
		v := m
		return &v, nil
	}
	return nil, fmt.Errorf("cachelayer: model code %q: %w", code, errNotFound)
}

// GetModelByCodeOrAlias returns the servable Model row whose customer-facing
// code OR one of its aliases matches key, resolved in O(1) from the in-memory
// code-or-alias index (no per-request scan, no DB read). Codes take priority
// over aliases. Used by the routing passthrough fallback so an aliased model
// routes without an explicit routing rule; GetModelByCode stays strict-code.
// Servability is load-bearing here: the passthrough fallback builds its target
// straight from this row without a second provider check, so a model on a
// disabled provider must not be resolvable or the call is dispatched upstream
// to a provider the operator turned off.
func (l *Layer) GetModelByCodeOrAlias(ctx context.Context, key string) (*store.Model, error) {
	idx := l.modelsByCodeOrAlias.Load()
	if idx == nil {
		return nil, fmt.Errorf("cachelayer: model code/alias %q: %w", key, ErrIndexUnavailable)
	}
	if m, ok := (*idx)[key]; ok {
		v := m
		return &v, nil
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
// freely between the two — including its deliberate omission of the
// provider-enabled check, which keeps a rule that redirects away from a
// disabled provider matchable (see store.DB.ResolveModelCandidates).
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

// ListEnabledModels returns a deterministic ordered slice of servable
// models (provider then name), matching store.DB.ListEnabledModels —
// including its exclusion of models whose provider is disabled.
func (l *Layer) ListEnabledModels(ctx context.Context) ([]store.Model, error) {
	all := l.models.All()
	out := make([]store.Model, 0, len(all))
	for _, m := range all {
		if m.Servable() {
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

// GetVirtualKeyByHash looks up a VK by its HMAC hash. Cache miss falls
// through to the database.
//
// A hash with no matching row is remembered as a negative result for
// VKTTL. Without it every request bearing an unknown or revoked key
// re-queried Postgres — an amplification path open to unauthenticated
// callers, and multiplied by the HMAC keyring version count because
// vkauth tries each version's hash in turn. Only "row absent" is cached;
// transport, scan and JSON-parse failures are transient and must keep
// retrying, so they fall through unchanged.
func (l *Layer) GetVirtualKeyByHash(ctx context.Context, hash string) (*store.VirtualKey, error) {
	if l.negativeVKHit(hash) {
		return nil, fmt.Errorf("cachelayer: virtual key not found (negative cache): %w", errNotFound)
	}
	vk, err := l.vkeys.Get(ctx, hash)
	if err != nil {
		if errors.Is(err, errNotFound) {
			l.recordNegativeVK(hash)
		}
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
// shape — one row per requested ID in the requested order, including the
// empty-pricing zero row for IDs missing from the snapshot. The caller
// resolves the selector's index against its own candidate slice, so the
// 1:1 ordering is part of the contract, not an implementation detail.
//
// Priced is the whole point of the row and must be set here. The float
// fields collapse "no price configured" and "priced at zero" onto the same
// 0.0, and quota.SelectCheapestIndex skips unpriced candidates on purpose —
// so a row that carries a real price but leaves Priced false is read as
// uncountable, every downgrade candidate is skipped, and the downgrade
// answers 429 "no affordable model available" with an affordable model in
// the list. This is the shipped path (Deps.Models is the cache layer), and
// only store.DB's copy of this function was setting the flag.
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
				mp.Priced = true
			}
			if m.OutputPricePM != nil {
				mp.OutputPricePM = *m.OutputPricePM
				mp.Priced = true
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
