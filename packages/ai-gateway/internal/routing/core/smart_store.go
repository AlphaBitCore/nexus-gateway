package core

import (
	"context"
	"fmt"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// SmartCatalog is the narrow surface the smart routing strategy needs
// from the catalog. Both *store.DB and *cachelayer.Layer satisfy it;
// production wires the cache layer so the per-request loop hits memory
// instead of PostgreSQL.
type SmartCatalog interface {
	ListEnabledModels(ctx context.Context) ([]store.Model, error)
	GetProvider(ctx context.Context, id string) (*store.Provider, error)
}

// smartStoreDB adapts a SmartCatalog into the SmartStore interface
// required by the smart routing strategy.
type smartStoreDB struct {
	db SmartCatalog
}

// NewSmartStoreDB creates a SmartStore backed by the given catalog
// reader. Production passes *cachelayer.Layer.
func NewSmartStoreDB(catalog SmartCatalog) SmartStore {
	return &smartStoreDB{db: catalog}
}

// ListEnabledChatModels returns all enabled chat models joined with their
// enabled providers. This excludes embedding models since smart routing
// is only for chat-type completions.
func (s *smartStoreDB) ListEnabledChatModels(ctx context.Context) ([]SmartModelRow, error) {
	models, err := s.db.ListEnabledModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("smart store: list models: %w", err)
	}

	providerCache := make(map[string]*store.Provider)

	var rows []SmartModelRow
	for _, m := range models {
		if m.Type != "chat" {
			continue
		}

		p, ok := providerCache[m.ProviderID]
		if !ok {
			var err error
			p, err = s.db.GetProvider(ctx, m.ProviderID)
			if err != nil {
				continue
			}
			providerCache[m.ProviderID] = p
		}
		if !p.Enabled {
			continue
		}

		row := SmartModelRow{
			ModelID:          m.ID,
			ModelCode:        m.Code,
			ModelName:        m.Name,
			ProviderID:       m.ProviderID,
			ProviderName:     p.Name,
			ProviderModelID:  m.ProviderModelID,
			InputPricePM:     m.InputPricePM,
			OutputPricePM:    m.OutputPricePM,
			Features:         m.Features,
			MaxContextTokens: m.MaxContextTokens,
			MaxOutputTokens:  m.MaxOutputTokens,
		}
		rows = append(rows, row)
	}

	return rows, nil
}
