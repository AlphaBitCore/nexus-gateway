package handler

import (
	"fmt"
	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/identity"
)

// ExemptionRebuilder is a type alias to config.ExemptionRebuilder so call
// sites that already reference handler.ExemptionRebuilder continue to compile.
type ExemptionRebuilder = config.ExemptionRebuilder

// ApplyActiveExemptions decodes the shadow state and rebuilds the proxy's
// in-memory ExemptionStore.
func ApplyActiveExemptions(s config.ExemptionRebuilder, state []byte) error {
	var v identity.ActiveExemptions
	if err := json.Unmarshal(state, &v); err != nil {
		return fmt.Errorf("exemptions: %w", err)
	}
	s.Rebuild(v.Entries)
	return nil
}
