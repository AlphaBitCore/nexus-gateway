package core

import "context"

// SmartStore defines the data access the smart strategy needs for
// candidate enumeration. Credential / base-URL lookup is delegated
// to [provtarget.Resolver] — the store only lists catalog rows.
type SmartStore interface {
	ListEnabledChatModels(ctx context.Context) ([]SmartModelRow, error)
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
