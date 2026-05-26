package cachestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/cacheconfig"
)

// GetCacheGlobalConfig returns the singleton global cache config row.
// If the row is somehow missing (shouldn't happen — seeded by migration),
// returns a zero-value GlobalConfig with all booleans false.
func (store *Store) GetCacheGlobalConfig(ctx context.Context) (cacheconfig.GlobalConfig, error) {
	var raw []byte
	err := store.pool.QueryRow(ctx,
		`SELECT config FROM cache_global_config WHERE id = 'singleton'`,
	).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return cacheconfig.GlobalConfig{}, nil
	}
	if err != nil {
		return cacheconfig.GlobalConfig{}, fmt.Errorf("get cache_global_config: %w", err)
	}
	var cfg cacheconfig.GlobalConfig
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return cacheconfig.GlobalConfig{}, fmt.Errorf("unmarshal cache_global_config: %w", err)
		}
	}
	return cfg, nil
}

// PutCacheGlobalConfig upserts the singleton row.
func (store *Store) PutCacheGlobalConfig(ctx context.Context, cfg cacheconfig.GlobalConfig, updatedBy string) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal cache_global_config: %w", err)
	}
	_, err = store.pool.Exec(ctx, `
		INSERT INTO cache_global_config (id, config, updated_at, updated_by)
		VALUES ('singleton', $1, NOW(), $2)
		ON CONFLICT (id) DO UPDATE SET config = $1, updated_at = NOW(), updated_by = $2
	`, raw, updatedBy)
	return err
}

// GetCacheAdapterConfig returns the Tier-2 row for the given adapter_type.
// Returns (zero, false, nil) if the row does not exist.
func (store *Store) GetCacheAdapterConfig(ctx context.Context, adapterType string) (cacheconfig.AdapterConfig, bool, error) {
	var raw []byte
	err := store.pool.QueryRow(ctx,
		`SELECT config FROM cache_adapter_config WHERE adapter_type = $1`, adapterType,
	).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return cacheconfig.AdapterConfig{}, false, nil
	}
	if err != nil {
		return cacheconfig.AdapterConfig{}, false, fmt.Errorf("get cache_adapter_config %q: %w", adapterType, err)
	}
	var cfg cacheconfig.AdapterConfig
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return cacheconfig.AdapterConfig{}, true, fmt.Errorf("unmarshal cache_adapter_config %q: %w", adapterType, err)
		}
	}
	return cfg, true, nil
}

// PutCacheAdapterConfig upserts the Tier-2 row for adapter_type.
func (store *Store) PutCacheAdapterConfig(ctx context.Context, adapterType string, cfg cacheconfig.AdapterConfig, updatedBy string) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal cache_adapter_config: %w", err)
	}
	_, err = store.pool.Exec(ctx, `
		INSERT INTO cache_adapter_config (adapter_type, config, updated_at, updated_by)
		VALUES ($1, $2, NOW(), $3)
		ON CONFLICT (adapter_type) DO UPDATE SET config = $2, updated_at = NOW(), updated_by = $3
	`, adapterType, raw, updatedBy)
	return err
}

// ListCacheAdapterConfigs returns every Tier-2 row keyed by adapter_type.
// Used by AssembleCacheConfigBlob.
func (store *Store) ListCacheAdapterConfigs(ctx context.Context) (map[string]cacheconfig.AdapterConfig, error) {
	rows, err := store.pool.Query(ctx, `SELECT adapter_type, config FROM cache_adapter_config`)
	if err != nil {
		return nil, fmt.Errorf("list cache_adapter_config: %w", err)
	}
	defer rows.Close()
	out := make(map[string]cacheconfig.AdapterConfig)
	for rows.Next() {
		var adapter string
		var raw []byte
		if err := rows.Scan(&adapter, &raw); err != nil {
			return nil, fmt.Errorf("scan cache_adapter_config row: %w", err)
		}
		var cfg cacheconfig.AdapterConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("unmarshal cache_adapter_config %q: %w", adapter, err)
			}
		}
		out[adapter] = cfg
	}
	return out, rows.Err()
}

// GetCacheProviderConfig returns the Tier-3 row for the given provider_id.
// Returns (zero, false, nil) if no override exists.
func (store *Store) GetCacheProviderConfig(ctx context.Context, providerID string) (cacheconfig.ProviderConfig, bool, error) {
	var raw []byte
	err := store.pool.QueryRow(ctx,
		`SELECT config FROM cache_provider_config WHERE provider_id = $1`, providerID,
	).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return cacheconfig.ProviderConfig{}, false, nil
	}
	if err != nil {
		return cacheconfig.ProviderConfig{}, false, fmt.Errorf("get cache_provider_config %q: %w", providerID, err)
	}
	var cfg cacheconfig.ProviderConfig
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return cacheconfig.ProviderConfig{}, true, fmt.Errorf("unmarshal cache_provider_config %q: %w", providerID, err)
		}
	}
	return cfg, true, nil
}

// PutCacheProviderConfig upserts the Tier-3 row. Caller must have already
// validated the body against the provider's adapter_type.
func (store *Store) PutCacheProviderConfig(ctx context.Context, providerID string, cfg cacheconfig.ProviderConfig, updatedBy string) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal cache_provider_config: %w", err)
	}
	_, err = store.pool.Exec(ctx, `
		INSERT INTO cache_provider_config (provider_id, config, updated_at, updated_by)
		VALUES ($1, $2, NOW(), $3)
		ON CONFLICT (provider_id) DO UPDATE SET config = $2, updated_at = NOW(), updated_by = $3
	`, providerID, raw, updatedBy)
	return err
}

// DeleteCacheProviderConfig removes the Tier-3 row entirely. Equivalent to
// "reset every field to inherit". Returns nil if the row did not exist.
func (store *Store) DeleteCacheProviderConfig(ctx context.Context, providerID string) error {
	_, err := store.pool.Exec(ctx, `DELETE FROM cache_provider_config WHERE provider_id = $1`, providerID)
	return err
}

// ListCacheProviderConfigs returns every Tier-3 row keyed by provider_id.
// Used by AssembleCacheConfigBlob and by /api/admin/cache/overrides.
func (store *Store) ListCacheProviderConfigs(ctx context.Context) (map[string]cacheconfig.ProviderConfig, error) {
	rows, err := store.pool.Query(ctx, `SELECT provider_id, config FROM cache_provider_config`)
	if err != nil {
		return nil, fmt.Errorf("list cache_provider_config: %w", err)
	}
	defer rows.Close()
	out := make(map[string]cacheconfig.ProviderConfig)
	for rows.Next() {
		var provider string
		var raw []byte
		if err := rows.Scan(&provider, &raw); err != nil {
			return nil, fmt.Errorf("scan cache_provider_config row: %w", err)
		}
		var cfg cacheconfig.ProviderConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("unmarshal cache_provider_config %q: %w", provider, err)
			}
		}
		out[provider] = cfg
	}
	return out, rows.Err()
}

// AssembleCacheConfigBlob reads all three tiers and packages them into the
// CacheConfigBlob shape that flows over the Hub shadow `cache` key.
// Called by every cache-mutating handler after persisting its tier-specific
// change, and by the reconcile job when comparing CP DB to thing.desired.
func (store *Store) AssembleCacheConfigBlob(ctx context.Context) (cacheconfig.CacheConfigBlob, error) {
	global, err := store.GetCacheGlobalConfig(ctx)
	if err != nil {
		return cacheconfig.CacheConfigBlob{}, err
	}
	adapters, err := store.ListCacheAdapterConfigs(ctx)
	if err != nil {
		return cacheconfig.CacheConfigBlob{}, err
	}
	providers, err := store.ListCacheProviderConfigs(ctx)
	if err != nil {
		return cacheconfig.CacheConfigBlob{}, err
	}
	return cacheconfig.CacheConfigBlob{
		Global:    global,
		Adapters:  adapters,
		Providers: providers,
	}, nil
}

// GetProviderAdapterType returns the adapter_type for a Provider, or
// ("", false, nil) if the Provider does not exist.
func (store *Store) GetProviderAdapterType(ctx context.Context, providerID string) (string, bool, error) {
	var adapter string
	err := store.pool.QueryRow(ctx,
		`SELECT adapter_type FROM "Provider" WHERE id = $1`, providerID,
	).Scan(&adapter)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get provider adapter_type %q: %w", providerID, err)
	}
	return adapter, true, nil
}

// GetProviderName returns just the name for a Provider (used by the
// /api/admin/cache/effective and /overrides UI surfaces).
func (store *Store) GetProviderName(ctx context.Context, providerID string) (string, error) {
	var name string
	err := store.pool.QueryRow(ctx, `SELECT name FROM "Provider" WHERE id = $1`, providerID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get provider name %q: %w", providerID, err)
	}
	return name, nil
}
