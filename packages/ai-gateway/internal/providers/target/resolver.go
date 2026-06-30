// Package provtarget resolves a (providerID, modelID) pair into a
// fully populated [provcore.CallTarget]. It is the single entry point
// for every internal caller that needs to invoke a provider adapter —
// the target executor, the smart router's router-LLM call, and the
// AI Guard `configured_provider` backend.
//
// The package exists in its own directory (rather than under
// providers/) so that it can depend on providers, credential, and
// health stores without inducing an import cycle.
package provtarget

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/credentials/pool"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// ResolveHints lets callers influence target selection when more than
// one option exists. All fields are optional.
type ResolveHints struct {
	// StickyKey is a discriminator (typically the virtual key ID) used for
	// consistent hashing when multiple credentials are available, so the same
	// caller always routes to the same credential and maximises provider-side
	// prompt-cache hits. Empty = weighted-random fallback.
	StickyKey string
	// Deployment, when non-empty, pins the Azure OpenAI deployment name
	// returned in CallTarget.Extras["azure.apiVersion"] / ProviderModelID.
	Deployment string
}

// Resolver produces a provider [provcore.CallTarget] ready to pass
// into [provcore.Adapter.Execute]. Implementations must populate
// ProviderName, BaseURL, APIKey, and ProviderModelID at minimum.
// Implementations MUST NOT log secrets.
type Resolver interface {
	Resolve(ctx context.Context, providerID, modelID string, hints ResolveHints) (provcore.CallTarget, error)
}

// ProviderStore is the minimum provider-catalog surface the default
// [PgResolver] needs. Decoupled so tests and the admin handler can
// share implementations.
type ProviderStore interface {
	GetProviderByID(ctx context.Context, providerID string) (ProviderRow, error)
}

// ProviderRow is the projection of the provider catalog row used to
// assemble a CallTarget. Concrete stores must populate the embedded
// Extras map with any provider-specific config (Azure API version,
// AWS region, GCP project/location, etc.).
//
// AdapterType carries the explicit Provider.adapter_type column (one
// of the nine provcore.Format values). Resolve copies it into
// CallTarget.Format so callers never re-derive the wire adapter from
// the operator-facing name.
type ProviderRow struct {
	ID          string
	Name        string
	AdapterType string
	BaseURL     string
	Extras      map[string]string
	Disabled    bool
	// ServesResponsesAPI mirrors Provider.serves_responses_api. nil = use
	// the adapter RequestShapes default; copied onto CallTarget so the
	// executor/bridge can resolve the /v1/responses capability without a
	// per-request DB read.
	ServesResponsesAPI *bool
}

// ModelStore is the minimum model-catalog surface the default
// [PgResolver] needs.
type ModelStore interface {
	GetModelByID(ctx context.Context, modelID string) (ModelRow, error)
}

// ModelRow is the projection of the model catalog row used to assemble
// a CallTarget.
type ModelRow struct {
	ID              string
	ProviderID      string
	ProviderModelID string
	Disabled        bool
}

// CredentialCandidate is one entry in the multi-credential pool.
type CredentialCandidate struct {
	ID     string
	Name   string
	Weight int
}

// CredentialStore resolves credentials for a provider.
type CredentialStore interface {
	// ResolveForProvider returns the decrypted API key and identity for the
	// credential identified by credentialID. credentialID must be a non-empty
	// credential UUID belonging to providerID — passing an empty string falls
	// back to the single-default path and is used only internally by
	// resolveCredential when ListForProvider returns no candidates.
	ResolveForProvider(ctx context.Context, providerID, credentialID string) (apiKey string, credID string, credName string, err error)
	// ListForProvider returns all eligible candidates for pool selection.
	// Implementors should filter to enabled + active + weight > 0.
	ListForProvider(ctx context.Context, providerID string) ([]CredentialCandidate, error)
}

// PgResolver is the production [Resolver] backed by the real provider,
// model, and credential stores. All three dependencies are required;
// nil dependencies cause Resolve to return an error rather than panic,
// so tests can exercise the error path without wiring a full stack.
// Redis is optional; when nil, circuit state is not consulted during pool
// selection (all candidates treated as closed/healthy).
type PgResolver struct {
	Providers   ProviderStore
	Models      ModelStore
	Credentials CredentialStore
	Redis       redis.Cmdable // optional; nil = no circuit awareness
}

// NewPgResolver constructs a [PgResolver].
func NewPgResolver(providers ProviderStore, models ModelStore, credentials CredentialStore) *PgResolver {
	return &PgResolver{Providers: providers, Models: models, Credentials: credentials}
}

// Resolve looks up the provider, model, and credential records and
// assembles a CallTarget. Missing or disabled records surface as
// errors so the executor can move on to the next target.
func (r *PgResolver) Resolve(ctx context.Context, providerID, modelID string, hints ResolveHints) (provcore.CallTarget, error) {
	if r == nil || r.Providers == nil || r.Models == nil || r.Credentials == nil {
		return provcore.CallTarget{}, fmt.Errorf("provtarget: resolver not fully wired")
	}
	pr, err := r.Providers.GetProviderByID(ctx, providerID)
	if err != nil {
		return provcore.CallTarget{}, fmt.Errorf("provtarget: provider %q: %w", providerID, err)
	}
	if pr.Disabled {
		return provcore.CallTarget{}, fmt.Errorf("provtarget: provider %q disabled", providerID)
	}
	if pr.AdapterType == "" {
		return provcore.CallTarget{}, fmt.Errorf("provtarget: provider %q has empty adapter_type", providerID)
	}
	mr, err := r.Models.GetModelByID(ctx, modelID)
	if err != nil {
		return provcore.CallTarget{}, fmt.Errorf("provtarget: model %q: %w", modelID, err)
	}
	if mr.Disabled {
		return provcore.CallTarget{}, fmt.Errorf("provtarget: model %q disabled", modelID)
	}
	if mr.ProviderID != providerID {
		return provcore.CallTarget{}, fmt.Errorf("provtarget: model %q belongs to provider %q, not %q", modelID, mr.ProviderID, providerID)
	}

	apiKey, credID, credName, err := r.resolveCredential(ctx, providerID, hints)
	if err != nil {
		return provcore.CallTarget{}, fmt.Errorf("provtarget: credential: %w", err)
	}

	target := provcore.CallTarget{
		ProviderID:         pr.ID,
		ProviderName:       pr.Name,
		Format:             provcore.Format(pr.AdapterType),
		BaseURL:            pr.BaseURL,
		APIKey:             apiKey,
		CredentialID:       credID,
		CredentialName:     credName,
		ProviderModelID:    mr.ProviderModelID,
		ServesResponsesAPI: pr.ServesResponsesAPI,
	}
	if len(pr.Extras) > 0 {
		target.Extras = make(map[string]string, len(pr.Extras))
		for k, v := range pr.Extras {
			target.Extras[k] = v
		}
	}
	if hints.Deployment != "" {
		if target.Extras == nil {
			target.Extras = make(map[string]string, 1)
		}
		target.Extras["azure.deployment"] = hints.Deployment
	}
	return target, nil
}

// resolveCredential picks a credential for providerID according to hints.
// It lists all eligible candidates, applies circuit-state from Redis,
// and uses credpool.Select with hints.StickyKey for consistent routing.
func (r *PgResolver) resolveCredential(ctx context.Context, providerID string, hints ResolveHints) (apiKey, credID, credName string, err error) {
	candidates, err := r.Credentials.ListForProvider(ctx, providerID)
	if err != nil || len(candidates) == 0 {
		// Fall back to single-credential path on error or empty list.
		return r.Credentials.ResolveForProvider(ctx, providerID, "")
	}
	if len(candidates) == 1 {
		return r.Credentials.ResolveForProvider(ctx, providerID, candidates[0].ID)
	}

	// Build pool entries and fetch circuit states.
	entries := make([]credpool.Entry, len(candidates))
	for i, c := range candidates {
		entries[i] = credpool.Entry{ID: c.ID, Weight: c.Weight}
	}
	if r.Redis != nil {
		ids := make([]string, len(candidates))
		for i, c := range candidates {
			ids[i] = c.ID
		}
		states := credpool.BulkCircuitStates(ctx, r.Redis, ids)
		for i := range entries {
			entries[i].Circuit = states[entries[i].ID]
		}
	}

	winner := credpool.Select(entries, hints.StickyKey)
	if winner == nil {
		return "", "", "", fmt.Errorf("all credentials for provider %q are circuit-open", providerID)
	}
	return r.Credentials.ResolveForProvider(ctx, providerID, winner.ID)
}
