package core

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// SmartStore defines the data access the smart strategy needs for
// candidate enumeration. Credential / base-URL lookup is delegated
// to [provtarget.Resolver] — the store only lists catalog rows.
type SmartStore interface {
	// ListEnabledChatModels returns the enabled chat models — the candidate
	// set for the LLM task-router on the chat/responses endpoints.
	ListEnabledChatModels(ctx context.Context) ([]SmartModelRow, error)
	// ListEnabledCandidates returns the enabled models whose modality can
	// serve the given endpoint kind (typology.EndpointKindAcceptsModelType).
	// It powers modality-aware `model=auto`: on /v1/images/generations it
	// yields image models, on the audio endpoints audio models, and so on, so
	// auto never crosses modality.
	ListEnabledCandidates(ctx context.Context, kind typology.EndpointKind) ([]SmartModelRow, error)
}

// SmartModelRow is a joined model+provider row returned by SmartStore.
type SmartModelRow struct {
	// ModelID is the Model row's UUID PK — used for FK references and
	// the final RoutingTarget. Not exposed to the LLM router because
	// 36-char UUIDs blow up token budget and LLMs frequently mistype
	// them.
	ModelID string
	// ModelCode is the customer-facing identifier ("gpt-4o"). Sent to
	// the LLM in the model catalog so it returns a short, recognisable
	// token; mapped back to ModelID after the LLM responds.
	ModelCode        string
	ModelName        string
	ProviderID       string
	ProviderName     string
	ProviderModelID  string
	InputPricePM     *float64
	OutputPricePM    *float64
	Features         []string
	MaxContextTokens *int
	MaxOutputTokens  *int
}
