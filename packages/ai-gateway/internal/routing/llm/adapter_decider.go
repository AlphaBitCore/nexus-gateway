package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// AdapterLookup resolves a wire-format key to a registered provider
// adapter. Defined as an interface here so AdapterDecider does not depend
// on the concrete *provcore.Registry type; *provcore.Registry satisfies
// the contract.
type AdapterLookup interface {
	Get(format provcore.Format) (provcore.Adapter, bool)
}

// AdapterDecider is the production Decider implementation. It resolves
// the router LLM's CallTarget via a provtarget.Resolver, picks the
// matching provider Adapter, builds the canonical OpenAI request body,
// calls the upstream, and parses the response.
type AdapterDecider struct {
	resolver provtarget.Resolver
	adapters AdapterLookup
	logger   *slog.Logger
}

// NewAdapterDecider constructs the production Decider.
func NewAdapterDecider(resolver provtarget.Resolver, adapters AdapterLookup, logger *slog.Logger) *AdapterDecider {
	return &AdapterDecider{
		resolver: resolver,
		adapters: adapters,
		logger:   logger,
	}
}

// Decide runs the full router-LLM call pipeline. Error text must remain
// byte-identical to the audit routing_trace vocabulary for the same
// failure modes.
func (a *AdapterDecider) Decide(ctx context.Context, req Request) (Decision, error) {
	target, err := a.resolver.Resolve(ctx, req.RouterProviderID, req.RouterModelID, provtarget.ResolveHints{})
	if err != nil {
		a.logger.Warn("smart: router target resolve failed", "error", err)
		return Decision{}, fmt.Errorf("router target resolve failed: %w", err)
	}
	if !target.Format.Valid() {
		return Decision{}, fmt.Errorf("invalid adapter_type on router provider %q (%q)", target.ProviderName, target.Format)
	}
	adapter, ok := a.adapters.Get(target.Format)
	if !ok {
		return Decision{}, fmt.Errorf("no adapter for router provider %q (format %q)", target.ProviderName, target.Format)
	}

	body := BuildRequestBody(target.ProviderModelID, req)
	bodyBytes, _ := json.Marshal(body)

	callCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()

	resp, err := adapter.Execute(callCtx, provcore.Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
		Body:       bodyBytes,
		Stream:     false,
		Target:     target,
	})
	if err != nil {
		isTimeout := callCtx.Err() != nil
		a.logger.Warn("smart: router LLM call failed", "error", err, "timeout", isTimeout)
		if isTimeout {
			return Decision{}, fmt.Errorf("router LLM timeout (%dms)", req.Timeout.Milliseconds())
		}
		return Decision{}, fmt.Errorf("router LLM error: %w", err)
	}
	if resp.StatusCode >= 400 {
		a.logger.Warn("smart: router LLM returned error", "status", resp.StatusCode, "provider", target.ProviderName)
		return Decision{}, fmt.Errorf("router LLM error: %d", resp.StatusCode)
	}

	d, err := ParseResponse(string(resp.Body))
	if err != nil {
		a.logger.Warn("smart: failed to parse router response", "error", err)
		return Decision{}, fmt.Errorf("failed to parse router response")
	}
	return d, nil
}
