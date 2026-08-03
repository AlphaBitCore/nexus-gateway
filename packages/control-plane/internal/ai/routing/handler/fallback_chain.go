package routing

import (
	"fmt"

	"github.com/goccy/go-json"
)

// fallbackChainEntry mirrors the AI Gateway's core.FallbackChainEntry — the
// struct the resolver unmarshals this column into. The resolver's decode is
// best-effort (a malformed chain silently yields ZERO recovery targets while
// the primary still routes), so without a write-time check an admin could
// persist a chain of bare model strings and lose all failover coverage with
// no signal. Reject the wrong shape here instead.
type fallbackChainEntry struct {
	ProviderID string `json:"providerId"`
	ModelID    string `json:"modelId"`
}

// validateFallbackChain returns ("", true) when raw is nil/null/empty-array or
// a JSON array of {providerId, modelId} objects with non-empty values;
// (msg, false) otherwise.
func validateFallbackChain(raw json.RawMessage) (string, bool) {
	s := string(raw)
	if len(raw) == 0 || s == "null" {
		return "", true
	}
	var chain []fallbackChainEntry
	if err := json.Unmarshal(raw, &chain); err != nil {
		return "fallbackChain must be a JSON array of {providerId, modelId} objects", false
	}
	for i, e := range chain {
		if e.ProviderID == "" || e.ModelID == "" {
			return fmt.Sprintf("fallbackChain[%d] requires a non-empty providerId and modelId", i), false
		}
	}
	return "", true
}
