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
	ModelCode       string
	ModelName       string
	ProviderID      string
	ProviderName    string
	ProviderModelID string
	InputPricePM    *float64
	OutputPricePM   *float64
	Features        []string
	// InputModalities is what the model accepts, and the only thing consulted
	// for a modality question — by the capability filter and by the router
	// LLM's catalog alike. `Features` used to carry a "vision" entry saying
	// the same thing in a second vocabulary, and the two disagreed on 34
	// production rows; it is no longer stored. Features keeps the
	// capabilities that are not modalities (streaming, function_calling,
	// json_mode, thinking).
	//
	// Not all of them are routing CONSTRAINTS, and `streaming` is the one that
	// looks like it should be. It discriminates — the rows without it are the
	// embedding, image, video and speech models, which is a real fact — but no
	// routing path reads it, and none should: whether a wire can stream is the
	// adapter's business, not a reason to prefer one target over another. It is
	// published by the model catalogue because callers read it to decide
	// whether to ask for a stream, and that is the whole of its job.
	InputModalities []string
	// RequiredModalities is the model's floor. A request that carries none of
	// these cannot be served by this model at all — gpt-audio-mini accepts
	// text and requires audio, and a text-only request routed to it is a
	// measured upstream 400. Empty is the normal case.
	RequiredModalities []string
	MaxContextTokens   *int
	MaxOutputTokens    *int
}
