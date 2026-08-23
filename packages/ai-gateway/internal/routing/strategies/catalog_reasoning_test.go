package strategies

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCatalog_ReasoningIsAPropertyOfTheModelNotOfItsHost.
//
// Whether a model reasons before answering is a fact about the MODEL. Who
// serves it — OpenAI directly, Azure, Bedrock, Vertex, an inference host —
// changes the endpoint and the price, not that.
//
// The catalogue disagreed with itself on exactly that: `azure-openai` tagged
// azure-gpt-5.6-*, azure-gpt-5.5, azure-gpt-5.4* and azure-o1/o3/o4-mini as
// reasoning, while `openai` tagged NONE of the same models. Harmless while
// nothing read the flag — and the moment reasoning becomes a routing
// constraint, a request that asked to reason has every OpenAI model filtered
// out of its pool while the identical model under azure survives, with nothing
// on the trace to explain it.
//
// Compared on the model name with the host's own prefix stripped, because that
// is how the same model is spelled twice in this file.
func TestCatalog_ReasoningIsAPropertyOfTheModelNotOfItsHost(t *testing.T) {
	// Prefixes a host puts on a model that is not its own. Each is a naming
	// convention in this catalogue, not a guess about the vendor.
	hostPrefixes := []string{"azure-", "bedrock-", "vertex-"}

	type where struct {
		provider string
		code     string
	}
	seen := map[string][]struct {
		at        where
		reasoning bool
	}{}

	for _, p := range loadRawCatalog(t).Providers {
		for _, m := range p.Models {
			if m.Type != "chat" {
				continue
			}
			base := m.Code
			for _, pre := range hostPrefixes {
				base = strings.TrimPrefix(base, pre)
			}
			if base == m.Code && p.Key != "openai" && p.Key != "anthropic" && p.Key != "google-gemini" {
				// Only compare a re-hosted model against its home. A code with
				// no host prefix on a third-party inference provider is that
				// provider's own naming and proves nothing about identity.
				continue
			}
			reasoning := false
			for _, f := range m.Features {
				if f == "reasoning" {
					reasoning = true
				}
			}
			seen[base] = append(seen[base], struct {
				at        where
				reasoning bool
			}{where{p.Key, m.Code}, reasoning})
		}
	}

	compared := 0
	for base, rows := range seen {
		if len(rows) < 2 {
			continue
		}
		compared++
		first := rows[0]
		for _, r := range rows[1:] {
			if r.reasoning != first.reasoning {
				t.Errorf("%q is tagged reasoning=%v as %s/%s and reasoning=%v as %s/%s — whether a "+
					"model reasons is a fact about the model, and disagreeing about it filters one "+
					"copy out of a pool the other survives",
					base, first.reasoning, first.at.provider, first.at.code,
					r.reasoning, r.at.provider, r.at.code)
			}
		}
	}
	if compared == 0 {
		t.Fatal("no model appeared under two hosts, so this compared nothing — the prefix list " +
			"or the catalogue's naming moved and the check is now vacuous")
	}
}

type rawCatalogModel struct {
	Code     string   `json:"code"`
	Type     string   `json:"type"`
	Features []string `json:"features"`
}

type rawCatalogProvider struct {
	Key    string            `json:"key"`
	Models []rawCatalogModel `json:"models"`
}

type rawCatalogFile struct {
	Providers []rawCatalogProvider `json:"providers"`
}

func loadRawCatalog(t *testing.T) rawCatalogFile {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "..", "tools", "db-migrate", "model-catalog.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shipped catalog: %v (looked at %s)", err, path)
	}
	var cf rawCatalogFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		t.Fatalf("parse shipped catalog: %v", err)
	}
	if len(cf.Providers) == 0 {
		t.Fatal("catalogue parsed to zero providers; the shape moved and every check over it passes")
	}
	return cf
}
