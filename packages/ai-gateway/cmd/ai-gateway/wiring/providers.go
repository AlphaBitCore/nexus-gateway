// provcore.go — provider adapter registry wiring.
//
// Bridges the cachelayer.Layer (in-memory snapshot caches) and the
// *credmanager.Manager (decrypt cache) to the minimal interfaces that
// provtarget.PgResolver depends on (ProviderStore, ModelStore,
// CredentialStore). Kept as a thin adapter so the provtarget package
// stays free of cache and DB imports.
package wiring

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/redis/go-redis/v9"

	geminicache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/gemini"
	cachelayer "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/layer"
	credmanager "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/credentials/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
)

// InitProviderRegistry builds and freezes the adapter registry.
func InitProviderRegistry(allowlist *forwardheader.Resolved, logger *slog.Logger) *provcore.Registry {
	adapterReg := provcore.NewRegistry()
	provbuiltins.Register(adapterReg, allowlist, logger)
	adapterReg.Freeze()
	return adapterReg
}

// InitForwardHeaderAllowlist resolves the forward-header allowlist from
// config and seeds the live atomic snapshot. Returns the resolved allowlist.
func InitForwardHeaderAllowlist(fhCfg forwardheader.Config) (*forwardheader.Resolved, error) {
	fhFormats := make([]string, 0, len(provcore.AllFormats()))
	for _, f := range provcore.AllFormats() {
		fhFormats = append(fhFormats, string(f))
	}
	allowlist, err := forwardheader.Resolve(fhCfg, fhFormats)
	if err != nil {
		return nil, err
	}
	forwardheader.SetActive(allowlist)
	return allowlist, nil
}

// providerStoreAdapter satisfies provtarget.ProviderStore via cachelayer.
type providerStoreAdapter struct{ layer *cachelayer.Layer }

func (p *providerStoreAdapter) GetProviderByID(ctx context.Context, providerID string) (provtarget.ProviderRow, error) {
	prov, err := p.layer.GetProvider(ctx, providerID)
	if err != nil {
		return provtarget.ProviderRow{}, err
	}
	extras := map[string]string{}
	if prov.Region != nil && *prov.Region != "" {
		extras["provider.region"] = *prov.Region
	}
	if prov.APIVersion != nil && *prov.APIVersion != "" {
		extras["provider.apiVersion"] = *prov.APIVersion
		extras["azure.apiVersion"] = *prov.APIVersion
	}
	if prov.PathPrefix != "" {
		extras["provider.pathPrefix"] = prov.PathPrefix
	}
	return provtarget.ProviderRow{
		ID:          prov.ID,
		Name:        prov.Name,
		AdapterType: prov.AdapterType,
		BaseURL:     prov.BaseURL,
		Extras:      extras,
		Disabled:    !prov.Enabled,
	}, nil
}

// modelStoreAdapter satisfies provtarget.ModelStore via cachelayer.
type modelStoreAdapter struct{ layer *cachelayer.Layer }

func (m *modelStoreAdapter) GetModelByID(ctx context.Context, modelID string) (provtarget.ModelRow, error) {
	mod, err := m.layer.GetModel(ctx, modelID)
	if err != nil {
		return provtarget.ModelRow{}, err
	}
	return provtarget.ModelRow{
		ID:              mod.ID,
		ProviderID:      mod.ProviderID,
		ProviderModelID: mod.ProviderModelID,
		Disabled:        !mod.Enabled,
	}, nil
}

// credentialStoreAdapter satisfies provtarget.CredentialStore via credmanager.Manager.
type credentialStoreAdapter struct{ mgr *credmanager.Manager }

func (c *credentialStoreAdapter) ResolveForProvider(ctx context.Context, providerID, credentialID string) (string, string, string, error) {
	if credentialID != "" {
		apiKey, err := c.mgr.GetDecrypted(ctx, credentialID)
		if err != nil {
			return "", "", "", fmt.Errorf("credential %q: %w", credentialID, err)
		}
		return apiKey, credentialID, "", nil
	}
	return c.mgr.GetForProvider(ctx, providerID)
}

func (c *credentialStoreAdapter) ListForProvider(ctx context.Context, providerID string) ([]provtarget.CredentialCandidate, error) {
	creds, err := c.mgr.ListForProvider(ctx, providerID)
	if err != nil {
		return nil, err
	}
	out := make([]provtarget.CredentialCandidate, len(creds))
	for i, cr := range creds {
		out[i] = provtarget.CredentialCandidate{
			ID:     cr.ID,
			Name:   cr.Name,
			Weight: cr.SelectionWeight,
		}
	}
	return out, nil
}

// NewResolver constructs the production provtarget.Resolver from the
// shared cache layer + credential manager. Either dependency may be nil
// during degraded startup; callers must guard against a nil return.
// rdb is optional; when non-nil it enables circuit-state awareness in
// multi-credential pool selection.
func NewResolver(layer *cachelayer.Layer, credMgr *credmanager.Manager, rdb redis.Cmdable) *provtarget.PgResolver {
	if layer == nil || credMgr == nil {
		return nil
	}
	r := provtarget.NewPgResolver(
		&providerStoreAdapter{layer: layer},
		&modelStoreAdapter{layer: layer},
		&credentialStoreAdapter{mgr: credMgr},
	)
	r.Redis = rdb
	return r
}

// GeminiKeyResolverFrom constructs a geminicache.KeyResolver backed by the
// provider store and credential manager. Returns nil when either dependency
// is absent (degraded startup without DB).
func GeminiKeyResolverFrom(layer *cachelayer.Layer, credMgr *credmanager.Manager) geminicache.KeyResolver {
	if layer == nil || credMgr == nil {
		return nil
	}
	return &geminiKeyResolverAdapter{
		providers: &providerStoreAdapter{layer: layer},
		creds:     &credentialStoreAdapter{mgr: credMgr},
	}
}

type geminiKeyResolverAdapter struct {
	providers provtarget.ProviderStore
	creds     provtarget.CredentialStore
}

func (a *geminiKeyResolverAdapter) Resolve(ctx context.Context, providerID, _ string) (apiKey, baseURL string, err error) {
	pr, err := a.providers.GetProviderByID(ctx, providerID)
	if err != nil {
		return "", "", fmt.Errorf("geminicache: provider %q: %w", providerID, err)
	}
	apiKey, _, _, err = a.creds.ResolveForProvider(ctx, providerID, "")
	if err != nil {
		return "", "", fmt.Errorf("geminicache: credential for %q: %w", providerID, err)
	}
	return apiKey, pr.BaseURL, nil
}
