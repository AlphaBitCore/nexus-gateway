package cachelayer

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// loadProviders reads every Provider row.
func (l *Layer) loadProviders(ctx context.Context) (map[string]store.Provider, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT id, name, "displayName", adapter_type, "baseUrl", "pathPrefix", "apiVersion", region, enabled, serves_responses_api
		FROM "Provider"
	`)
	if err != nil {
		return nil, fmt.Errorf("cachelayer: load providers: %w", err)
	}
	defer rows.Close()

	out := map[string]store.Provider{}
	for rows.Next() {
		var p store.Provider
		if err := rows.Scan(&p.ID, &p.Name, &p.DisplayName, &p.AdapterType, &p.BaseURL,
			&p.PathPrefix, &p.APIVersion, &p.Region, &p.Enabled, &p.ServesResponsesAPI); err != nil {
			return nil, fmt.Errorf("cachelayer: scan provider: %w", err)
		}
		out[p.ID] = p
	}
	return out, nil
}

// loadModels reads every Model row joined with provider denormalization
// fields the handler needs (provider name/displayName/baseURL).
func (l *Layer) loadModels(ctx context.Context) (map[string]store.Model, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT m.id, m.code, m.name, m."providerId", p.name, p.adapter_type, p."displayName", p."baseUrl",
		       m."providerModelId", m.type, m.enabled,
		       m."inputPricePerMillion", m."outputPricePerMillion",
		       m."cachedInputReadPricePerMillion", m."cachedInputWritePricePerMillion",
		       COALESCE(m.features, '{}'), m."maxContextTokens", m."maxOutputTokens",
		       COALESCE(m.aliases, '{}'),
		       COALESCE(m."inputModalities", '{}'), COALESCE(m."outputModalities", '{}'),
		       COALESCE(m.lifecycle, 'ga'), m."capabilityJson"
		FROM "Model" m
		LEFT JOIN "Provider" p ON p.id = m."providerId"
	`)
	if err != nil {
		return nil, fmt.Errorf("cachelayer: load models: %w", err)
	}
	defer rows.Close()

	byID := map[string]store.Model{}
	byCode := map[string]store.Model{}
	for rows.Next() {
		var m store.Model
		var inPrice, outPrice, cachedReadPrice, cachedWritePrice *string
		var maxCtx, maxOut pgtype.Int4
		if err := rows.Scan(&m.ID, &m.Code, &m.Name, &m.ProviderID, &m.ProviderName, &m.ProviderAdapterType, &m.ProviderDisplayName,
			&m.ProviderBaseURL, &m.ProviderModelID, &m.Type, &m.Enabled,
			&inPrice, &outPrice, &cachedReadPrice, &cachedWritePrice,
			&m.Features, &maxCtx, &maxOut, &m.Aliases,
			&m.InputModalities, &m.OutputModalities, &m.Lifecycle, &m.CapabilityJson); err != nil {
			return nil, fmt.Errorf("cachelayer: scan model: %w", err)
		}
		if f, ok := store.ParseDecimal(inPrice); ok {
			m.InputPricePM = &f
		}
		if f, ok := store.ParseDecimal(outPrice); ok {
			m.OutputPricePM = &f
		}
		if f, ok := store.ParseDecimal(cachedReadPrice); ok {
			m.CachedInputReadPricePM = &f
		}
		if f, ok := store.ParseDecimal(cachedWritePrice); ok {
			m.CachedInputWritePricePM = &f
		}
		if maxCtx.Valid {
			v := int(maxCtx.Int32)
			m.MaxContextTokens = &v
		}
		if maxOut.Valid {
			v := int(maxOut.Int32)
			m.MaxOutputTokens = &v
		}
		byID[m.ID] = m
		// Only enabled models are routable by code (matches GetModelByCode
		// historical filter). Disabled rows still live in byID for admin
		// lookups and quota pricing.
		if m.Enabled && m.Code != "" {
			byCode[m.Code] = m
		}
	}
	l.modelsByCode.Store(&byCode)
	return byID, nil
}

// loadCredentials reads every Credential row and builds the per-provider
// "first enabled, newest first" secondary index used by GetForProvider.
func (l *Layer) loadCredentials(ctx context.Context) (map[string]store.Credential, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT id, name, "providerId", "encryptedKey", "encryptionIv", "encryptionTag",
		       COALESCE(encryption_key_id, 'v1'), enabled, COALESCE("rotationState", 'none'),
		       COALESCE("selectionWeight", 100), COALESCE(status, 'active'),
		       "createdAt"
		FROM "Credential"
		ORDER BY
		  CASE WHEN COALESCE("rotationState", 'none') = 'pending_rotation' THEN 1 ELSE 0 END ASC,
		  "createdAt" DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("cachelayer: load credentials: %w", err)
	}
	defer rows.Close()

	byID := map[string]store.Credential{}
	byProvider := map[string]store.Credential{}
	for rows.Next() {
		var c store.Credential
		var createdAt pgtype.Timestamptz
		if err := rows.Scan(&c.ID, &c.Name, &c.ProviderID, &c.EncryptedKey,
			&c.EncryptionIv, &c.EncryptionTag, &c.EncryptionKeyID, &c.Enabled, &c.RotationState,
			&c.SelectionWeight, &c.Status,
			&createdAt); err != nil {
			return nil, fmt.Errorf("cachelayer: scan credential: %w", err)
		}
		byID[c.ID] = c
		// First enabled, active credential per provider wins (rows sorted DESC by createdAt).
		if c.Enabled && c.Status == "active" {
			if _, exists := byProvider[c.ProviderID]; !exists {
				byProvider[c.ProviderID] = c
			}
		}
	}
	l.credentialsByProviderFirst.Store(&byProvider)
	return byID, nil
}

// loadVirtualKey looks up a VK by its HMAC hash. Used as the per-key
// loader by the KeyCache; misses cache the not-found state? No — errors
// are not cached, so a not-found returns the underlying error and the
// next call retries.
func (l *Layer) loadVirtualKey(ctx context.Context, keyHash string) (*store.VirtualKey, error) {
	return l.db.GetVirtualKeyByHash(ctx, keyHash)
}
