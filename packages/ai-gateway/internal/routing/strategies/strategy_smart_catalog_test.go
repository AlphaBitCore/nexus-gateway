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
		{ModelID: "m-a1", ModelCode: "code-a1", ModelName: "A1", ProviderID: "p-1", ProviderName: "Prov One", ProviderModelID: "api-a1", Features: []string{"function_calling", "streaming"}},
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
	// Models are ordered newest-generation-first within a provider, so
	// "code-a2" precedes "code-a1" (natural-descending on the code).
	if groups[1].M[0].I != "code-a2" || groups[1].M[1].I != "code-a1" {
		t.Fatalf("p-1 models not newest-first: %+v", groups[1].M)
	}
	if groups[1].M[1].F == nil || len(groups[1].M[1].F) != 2 || groups[1].M[1].F[0] != "function_calling" {
		t.Fatalf("features on code-a1 (now second): %+v", groups[1].M[1].F)
	}
	if groups[1].M[0].IP == nil || *groups[1].M[0].IP != 0.1 {
		t.Fatalf("ip on code-a2 (now first): %+v", groups[1].M[0].IP)
	}
	if groups[1].M[0].MX == nil || *groups[1].M[0].MX != mx || groups[1].M[0].MO == nil || *groups[1].M[0].MO != mo {
		t.Fatalf("mx/mo on code-a2 (now first): mx=%v mo=%v", groups[1].M[0].MX, groups[1].M[0].MO)
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

func TestNaturalCodeLess_VersionOrdering(t *testing.T) {
	// Each pair: the first argument must sort strictly before the second.
	less := []struct{ a, b string }{
		{"claude-opus-4-6", "claude-opus-4-7"},
		{"claude-opus-4-7", "claude-opus-4-8"},
		{"gpt-5.4", "gpt-5.5"},
		{"kimi-k2.5", "kimi-k2.6"},
		{"gpt-4-turbo", "gpt-4o"},               // non-digit run tiebreak: '-' < 'o'
		{"claude-opus-4-8", "claude-opus-4-10"}, // 8 < 10 numerically, not lexically
		{"o1", "o3"},
	}
	for _, tc := range less {
		if !naturalCodeLess(tc.a, tc.b) {
			t.Errorf("naturalCodeLess(%q,%q)=false; want true", tc.a, tc.b)
		}
		if naturalCodeLess(tc.b, tc.a) {
			t.Errorf("naturalCodeLess(%q,%q)=true; want false (asymmetry)", tc.b, tc.a)
		}
	}
	if naturalCodeLess("gpt-4o", "gpt-4o") {
		t.Error("equal codes must not be less than each other")
	}
}

// The observed bug: the router picked the OLDEST same-tier generation. The
// catalog now lists each provider's models newest-first, so the newest
// generation (opus-4-8) appears before older ones (4-7, 4-6) — giving the
// router a primacy signal the plain-text recency rule alone did not provide.
func TestBuildModelCatalog_NewestGenerationFirst(t *testing.T) {
	in := []core.SmartModelRow{
		{ModelID: "m6", ModelCode: "claude-opus-4-6", ProviderID: "anthropic"},
		{ModelID: "m8", ModelCode: "claude-opus-4-8", ProviderID: "anthropic"},
		{ModelID: "m7", ModelCode: "claude-opus-4-7", ProviderID: "anthropic"},
	}
	raw := buildModelCatalog(in)
	var groups []struct {
		P string `json:"p"`
		M []struct {
			I string `json:"i"`
		} `json:"m"`
	}
	if err := json.Unmarshal([]byte(raw), &groups); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(groups) != 1 || len(groups[0].M) != 3 {
		t.Fatalf("unexpected catalog shape: %s", raw)
	}
	got := []string{groups[0].M[0].I, groups[0].M[1].I, groups[0].M[2].I}
	want := []string{"claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("catalog order = %v; want newest-first %v", got, want)
		}
	}
}

// The router LLM's only way to tell a vision model from a text-only one. It
// used to be the `vision` tag inside f; when vision stopped being stored, a
// prompt still asking for that tag would have matched nothing at all.
func TestBuildModelCatalog_CarriesNonTextInputModalities(t *testing.T) {
	out := buildModelCatalog([]core.SmartModelRow{
		{ModelID: "m1", ModelCode: "sees-images", ProviderID: "p1", InputModalities: []string{"text", "image"}},
	})
	if !strings.Contains(out, `"in"`) || !strings.Contains(out, `"image"`) {
		t.Errorf("catalog = %s; the router cannot tell this model accepts images", out)
	}
}

// Every model in the catalog accepts text, so spelling it out on every row
// would spend prompt budget on a constant. Measured on the seeded chat
// catalogue, carrying the arrays verbatim instead of the non-text remainder
// costs an extra ~40% on top of the +12% this encoding already adds.
func TestBuildModelCatalog_OmitsTextOnlyInput(t *testing.T) {
	out := buildModelCatalog([]core.SmartModelRow{
		{ModelID: "m1", ModelCode: "text-only", ProviderID: "p1", InputModalities: []string{"text"}},
	})
	if strings.Contains(out, `"in"`) {
		t.Errorf("catalog = %s; a text-only model should carry no modality key", out)
	}
}
