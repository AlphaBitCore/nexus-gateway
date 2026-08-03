package strategies

import (
	"sort"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// smartCatalogProvider is one provider group in the router system prompt JSON.
// Keys are intentionally short to reduce tokens; see llm.DefaultSystemPrompt.
type smartCatalogProvider struct {
	ProviderID string            `json:"p"`
	Models     []smartCatalogRow `json:"m"`
}

// smartCatalogRow is one routable model. JSON key i is Model.code only
// (the UUID Model.id and providerModelId are intentionally omitted from
// the catalog JSON — the router LLM is shown the short customer-facing code).
// ip/op USD per 1M tokens, f = feature tags, mx/mo = context and output limits.
type smartCatalogRow struct {
	ID       string   `json:"i"`
	InPM     *float64 `json:"ip,omitempty"`
	OutPM    *float64 `json:"op,omitempty"`
	Features []string `json:"f,omitempty"`
	MaxCtx   *int     `json:"mx,omitempty"`
	MaxOut   *int     `json:"mo,omitempty"`
}

// buildModelCatalog converts candidate rows into compact JSON for the system
// prompt: providers with nested models (i = Model.code, optional pricing and
// limits). We send the customer-facing code rather than the UUID so the LLM
// has a recognisable, short token to return; resolveSelectedModelID maps it
// back to a UUID for the runtime lookup.
func buildModelCatalog(candidates []core.SmartModelRow) string {
	order := make([]string, 0)
	seen := make(map[string]struct{})
	byProvider := make(map[string][]smartCatalogRow)
	for _, c := range candidates {
		if _, ok := seen[c.ProviderID]; !ok {
			seen[c.ProviderID] = struct{}{}
			order = append(order, c.ProviderID)
		}
		row := smartCatalogRow{ID: c.ModelCode}
		if c.InputPricePM != nil {
			row.InPM = c.InputPricePM
		}
		if c.OutputPricePM != nil {
			row.OutPM = c.OutputPricePM
		}
		if len(c.Features) > 0 {
			row.Features = c.Features
		}
		if c.MaxContextTokens != nil {
			row.MaxCtx = c.MaxContextTokens
		}
		if c.MaxOutputTokens != nil {
			row.MaxOut = c.MaxOutputTokens
		}
		byProvider[c.ProviderID] = append(byProvider[c.ProviderID], row)
	}
	out := make([]smartCatalogProvider, 0, len(order))
	for _, pid := range order {
		rows := byProvider[pid]
		// Order each provider's models newest-generation-first so the router
		// LLM sees the latest models at the top. The version digits in the
		// model code (opus-4-8 vs opus-4-6, gpt-5.5 vs gpt-5.4) are the only
		// reliable recency signal — createdAt ties across a seed batch and no
		// release-date column exists — so a natural (digit-aware) sort on the
		// code, descending, puts the newest of each family first. Combined with
		// the system prompt's "prefer the earliest-listed when equally capable"
		// rule, this fixes the router picking an older same-tier generation
		// (e.g. opus-4-6 over opus-4-8) that a plain text instruction alone did
		// not correct with the small router model.
		sort.SliceStable(rows, func(i, j int) bool {
			return naturalCodeLess(rows[j].ID, rows[i].ID) // reversed → descending
		})
		out = append(out, smartCatalogProvider{
			ProviderID: pid,
			Models:     rows,
		})
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b)
}

// naturalCodeLess reports whether model code a sorts before b in natural
// (human) order: maximal digit runs compare numerically (so "4-8" > "4-10"
// is false — 8 < 10), and non-digit runs compare byte-wise. This makes
// "claude-opus-4-6" < "claude-opus-4-7" < "claude-opus-4-8" and
// "gpt-5.4" < "gpt-5.5", giving buildModelCatalog a reliable newest-last
// baseline it reverses to newest-first.
func naturalCodeLess(a, b string) bool {
	for len(a) > 0 && len(b) > 0 {
		ad, bd := a[0] >= '0' && a[0] <= '9', b[0] >= '0' && b[0] <= '9'
		if ad && bd {
			// Compare two digit runs numerically via length-then-lexicographic
			// on the leading-zero-trimmed runs.
			ai, bi := digitRunEnd(a), digitRunEnd(b)
			ar, br := trimZeros(a[:ai]), trimZeros(b[:bi])
			if len(ar) != len(br) {
				return len(ar) < len(br)
			}
			if ar != br {
				return ar < br
			}
			a, b = a[ai:], b[bi:]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func digitRunEnd(s string) int {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return i
}

func trimZeros(s string) string {
	i := 0
	for i < len(s)-1 && s[i] == '0' {
		i++
	}
	return s[i:]
}
