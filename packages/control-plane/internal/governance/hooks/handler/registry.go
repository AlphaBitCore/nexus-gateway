package hooks

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/hooks/hookstore"
)

// hook implementation registry (mirrors UI constants/hooks.ts)

type hookRegistryEntry struct {
	Category        string
	Label           string
	SupportedStages []string
	DualPhase       bool
}

// HookImplRegistry mirrors the Go factory registrations in
// packages/shared/policy/hooks/registry.go and the ai-gateway-local factories
// (webhook-forward, quality-checker). If an implementationId is advertised
// here but not registered as a factory, PolicyResolver will warn-log-and-skip
// every row that uses it — so these two lists MUST stay in sync.
var HookImplRegistry = map[string]hookRegistryEntry{
	"pii-detector":           {Category: "compliance", Label: "Compliance & content safety", SupportedStages: []string{"request", "response"}, DualPhase: true},
	"keyword-filter":         {Category: "compliance", Label: "Compliance & content safety", SupportedStages: []string{"request", "response"}, DualPhase: true},
	"content-safety":         {Category: "compliance", Label: "Compliance & content safety", SupportedStages: []string{"request", "response"}, DualPhase: true},
	"rulepack-engine":        {Category: "compliance", Label: "Compliance & content safety", SupportedStages: []string{"request", "response"}, DualPhase: true},
	"data-residency":         {Category: "compliance", Label: "Compliance & content safety", SupportedStages: []string{"request", "response"}, DualPhase: true},
	"rate-limiter":           {Category: "traffic_control", Label: "Traffic & limits", SupportedStages: []string{"request"}, DualPhase: false},
	"request-size-validator": {Category: "traffic_control", Label: "Traffic & limits", SupportedStages: []string{"request"}, DualPhase: false},
	"ip-access-filter":       {Category: "traffic_control", Label: "Traffic & limits", SupportedStages: []string{"request"}, DualPhase: false},
	"quality-checker":        {Category: "quality", Label: "Quality & signals", SupportedStages: []string{"response"}, DualPhase: false},
	"webhook-forward":        {Category: "custom", Label: "Custom / other", SupportedStages: []string{"request", "response"}, DualPhase: true},
	"noop":                   {Category: "custom", Label: "Custom / other", SupportedStages: []string{"request", "response"}, DualPhase: true},
}

var hookCategoryLabels = map[string]string{
	"compliance":      "Compliance & content safety",
	"traffic_control": "Traffic & limits",
	"quality":         "Quality & signals",
	"observability":   "Observability",
	"custom":          "Custom / other",
}

// validHookStages / validHookFailBehaviors / validHookTypes are the
// enum whitelists for admin CRUD request payloads. Postgres enforces
// nothing at the column level — these columns are plain TEXT — so
// rejecting unknown values here is the only line of defense against
// garbage inputs bringing down the downstream pipeline builder.
var (
	validHookStages        = map[string]struct{}{"request": {}, "response": {}}
	validHookFailBehaviors = map[string]struct{}{"fail-open": {}, "fail-closed": {}}
	validHookTypes         = map[string]struct{}{"builtin": {}, "webhook": {}, "script": {}}
)

// deref returns the pointee or "" if p is nil.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ValidateHookEnums checks the four enum-shaped fields on a hook config
// payload. Empty strings are treated as "field omitted" and skipped — the
// caller is expected to fill in defaults before calling this helper.
// Returns a human-readable message suitable for a 400 response, or "" on ok.
// Exported so the parent handler/ package's cross-domain registry test
// (admin_hooks_registry_test.go) can drive it without re-implementing.
func ValidateHookEnums(stage, failBehavior, hookType, implementationID string) string {
	if stage != "" {
		if _, ok := validHookStages[stage]; !ok {
			return "stage must be 'request' or 'response'"
		}
	}
	if failBehavior != "" {
		if _, ok := validHookFailBehaviors[failBehavior]; !ok {
			return "failBehavior must be 'fail-open' or 'fail-closed'"
		}
	}
	if hookType != "" {
		if _, ok := validHookTypes[hookType]; !ok {
			return "type must be 'builtin', 'webhook', or 'script'"
		}
	}
	if implementationID != "" {
		if _, ok := HookImplRegistry[implementationID]; !ok {
			return "implementationId " + implementationID + " is not registered — see GET /admin/hook-implementations"
		}
	}
	return ""
}

// hookClassification builds the classification object the UI expects.
type hookClassification struct {
	Category            string   `json:"category"`
	CategoryLabel       string   `json:"categoryLabel"`
	CategorySource      string   `json:"categorySource"`
	ImplementationID    *string  `json:"implementationId"`
	ImplementationLabel *string  `json:"implementationLabel"`
	Phase               string   `json:"phase"`
	PhaseLabel          string   `json:"phaseLabel"`
	SupportedStages     []string `json:"supportedStages"`
	DualPhaseCapable    bool     `json:"dualPhaseCapable"`
}

// hookConfigWithClassification is the enriched response type.
type hookConfigWithClassification struct {
	hookstore.HookConfig
	Classification hookClassification `json:"classification"`
}

func classifyHook(hc *hookstore.HookConfig) hookClassification {
	entry, found := HookImplRegistry[hc.ImplementationID]

	// Determine category: DB override takes precedence.
	cat := "custom"
	catSource := "registry"
	if hc.Category != nil && *hc.Category != "" {
		cat = *hc.Category
		catSource = "database"
	} else if found {
		cat = entry.Category
	}

	catLabel := hookCategoryLabels[cat]
	if catLabel == "" {
		catLabel = "Custom / other"
	}

	supportedStages := []string{hc.Stage}
	dualPhase := false
	if found {
		supportedStages = entry.SupportedStages
		dualPhase = entry.DualPhase
	}

	phaseLabel := hc.Stage
	switch hc.Stage {
	case "request":
		phaseLabel = "Request"
	case "response":
		phaseLabel = "Response"
	}

	var implID *string
	var implLabel *string
	if hc.ImplementationID != "" {
		id := hc.ImplementationID
		implID = &id
		if found {
			lbl := entry.Label
			implLabel = &lbl
		}
	}

	return hookClassification{
		Category:            cat,
		CategoryLabel:       catLabel,
		CategorySource:      catSource,
		ImplementationID:    implID,
		ImplementationLabel: implLabel,
		Phase:               hc.Stage,
		PhaseLabel:          phaseLabel,
		SupportedStages:     supportedStages,
		DualPhaseCapable:    dualPhase,
	}
}

func enrichHook(hc *hookstore.HookConfig) hookConfigWithClassification {
	return hookConfigWithClassification{HookConfig: *hc, Classification: classifyHook(hc)}
}

func enrichHooks(hookList []hookstore.HookConfig) []hookConfigWithClassification {
	out := make([]hookConfigWithClassification, len(hookList))
	for i := range hookList {
		out[i] = enrichHook(&hookList[i])
	}
	return out
}
