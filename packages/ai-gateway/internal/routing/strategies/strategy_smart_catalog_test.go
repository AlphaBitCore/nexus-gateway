package strategies

import (
	"github.com/goccy/go-json"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

func TestBuildModelCatalog_GroupsByProviderShortKeys(t *testing.T) {
	mx := 128000
	mo := 4096
	in := []core.SmartModelRow{
		// Catalog `i` is Model.code, not the UUID id.
		{ModelID: "m-b", ModelCode: "code-b", ModelName: "B", ProviderID: "p-2", ProviderName: "Prov Two", ProviderModelID: "api-b"},
		{ModelID: "m-a1", ModelCode: "code-a1", ModelName: "A1", ProviderID: "p-1", ProviderName: "Prov One", ProviderModelID: "api-a1", Features: []string{"vision", "streaming"}},
		{ModelID: "m-a2", ModelCode: "code-a2", ModelName: "A2", ProviderID: "p-1", ProviderName: "Prov One", ProviderModelID: "api-a2", InputPricePM: fp(0.1), OutputPricePM: fp(0.2),
			MaxContextTokens: &mx, MaxOutputTokens: &mo},
	}
	raw := buildModelCatalog(in)
	for _, banned := range []string{`"name"`, `"provider"`, `"providerId"`, `"models"`, `"inputPricePerMillion"`, `"u":`} {
		if strings.Contains(raw, banned) {
			t.Fatalf("catalog must use compact keys, found %s in: %s", banned, raw)
		}
	}
	var groups []struct {
		P string `json:"p"`
		M []struct {
			I  string   `json:"i"`
			IP *float64 `json:"ip"`
			F  []string `json:"f"`
			MX *int     `json:"mx"`
			MO *int     `json:"mo"`
		} `json:"m"`
	}
	if err := json.Unmarshal([]byte(raw), &groups); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(groups) != 2 || groups[0].P != "p-2" || groups[1].P != "p-1" {
		t.Fatalf("unexpected provider order or count: %+v", groups)
	}
	if len(groups[0].M) != 1 || groups[0].M[0].I != "code-b" {
		t.Fatalf("p-2 models: %+v", groups[0].M)
	}
	if len(groups[1].M) != 2 {
		t.Fatalf("p-1 want 2 models, got %+v", groups[1].M)
	}
	if len(groups[1].M[0].F) != 2 || groups[1].M[0].F[0] != "vision" {
		t.Fatalf("features on first p-1 model: %+v", groups[1].M[0].F)
	}
	if groups[1].M[1].IP == nil || *groups[1].M[1].IP != 0.1 {
		t.Fatalf("ip on second p-1 model: %+v", groups[1].M[1].IP)
	}
	if groups[1].M[1].MX == nil || *groups[1].M[1].MX != mx || groups[1].M[1].MO == nil || *groups[1].M[1].MO != mo {
		t.Fatalf("mx/mo on second p-1 model: mx=%v mo=%v", groups[1].M[1].MX, groups[1].M[1].MO)
	}
}

func TestResolveSelectedModelID_ProviderScope(t *testing.T) {
	candidates := []core.SmartModelRow{
		{ModelID: "dup", ModelCode: "code-x", ModelName: "X", ProviderID: "p1", ProviderName: "one", ProviderModelID: "api-x"},
		{ModelID: "other", ModelCode: "code-y", ModelName: "Y", ProviderID: "p2", ProviderName: "two", ProviderModelID: "api-y"},
	}
	// providerModelId fallback path, scoped to a provider — happy path.
	id, ok := resolveSelectedModelID("api-y", "p2", candidates)
	if !ok || id != "other" {
		t.Fatalf("want other via providerModelId scoped to p2, got %q ok=%v", id, ok)
	}
	_, ok = resolveSelectedModelID("api-y", "p1", candidates)
	if ok {
		t.Fatal("wrong providerId should not match")
	}
	// ModelCode match (the LLM's canonical happy path) returns the
	// underlying UUID, not the code.
	id, ok = resolveSelectedModelID("code-y", "p2", candidates)
	if !ok || id != "other" {
		t.Fatalf("want other via code match scoped to p2, got %q ok=%v", id, ok)
	}
}

func fp(f float64) *float64 { return &f }
