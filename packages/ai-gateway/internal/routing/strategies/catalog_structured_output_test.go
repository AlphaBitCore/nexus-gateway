package strategies

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The catalog is the load-bearing half of the structured-output dimension: the
// filter is only as right as the rows it reads, and a row is edited by people
// who were not here when it was measured. So the measurement is pinned.
//
// Every entry below was PROBED on 2026-08-19 against the provider's own wire,
// one request per model, checking the response body and not just the status —
// evidence in ~/nexus/4xx-corpus/structured-outputs-probe-20260819.txt. Vendor
// documentation was not used, and neither was the sibling model: `json_mode`,
// the tag that already existed, disagrees with the wire on exactly the models
// that matter (gpt-4-turbo carries it and refuses json_schema; every claude-*
// and command-* lacks it and serves one).
//
// Adding a code here without probing it defeats the point of the file.
var structuredOutputProbe = map[string]bool{
	// OpenAI
	"gpt-4-turbo": false, // 400 'response_format' of type 'json_schema' is not supported
	"gpt-4.1":     true, "gpt-4.1-mini": true, "gpt-4.1-nano": true,
	"gpt-4o": true, "gpt-4o-mini": true,
	"gpt-5.4": true, "gpt-5.4-mini": true, "gpt-5.4-nano": true, "gpt-5.5": true,
	"gpt-5.6-luna": true, "gpt-5.6-sol": true, "gpt-5.6-terra": true,
	"o1": true, "o3": true, "o3-mini": true, "o4-mini": true,

	// Anthropic — via output_config.format, which its own 400 names.
	"claude-fable-5": true, "claude-haiku-4-5": true, "claude-opus-4-5": true,
	"claude-opus-4-6": true, "claude-opus-4-7": true, "claude-opus-4-8": true,
	"claude-opus-5": true, "claude-sonnet-4-5": true, "claude-sonnet-4-6": true,
	"claude-sonnet-5": true,

	// Gemini — via generationConfig.responseSchema.
	"gemini-2.5-flash": true, "gemini-2.5-flash-lite": true, "gemini-2.5-pro": true,
	"gemini-3.1-flash-lite": true, "gemini-3.5-flash": true,
	"gemini-3.5-flash-lite": true, "gemini-3.6-flash": true,

	// Cohere
	"command-a-03-2025": true, "command-a-vision-07-2025": true,
	"command-r-08-2024": true, "command-r-plus-08-2024": true, "command-r7b-12-2024": true,

	// Moonshot
	"kimi-k2.5": false, // HTTP 200, finish_reason=stop, PROSE — silently ignores the schema
	"kimi-k2.6": true, "kimi-k2.7-code": true, "kimi-k3": true,
	"moonshot-v1-8k": true, "moonshot-v1-32k": true, "moonshot-v1-128k": true,

	// DeepSeek
	"deepseek-v4-flash": false, // 400 This response_format type is unavailable now
	"deepseek-v4-pro":   false, // 400 This response_format type is unavailable now

	// Round 2, same day. The first sweep probed the model codes the replay
	// corpus named and left every OTHER enabled chat row untagged — and an
	// untagged row that declares any other feature is DROPPED by the filter,
	// because the row-level fail-open only rescues rows declaring nothing at
	// all. Six enabled rows were being excluded from structured-output routing
	// on no evidence either way; these are their measurements.
	"moonshot-v1-8k-vision-preview": true, "moonshot-v1-32k-vision-preview": true,
	"moonshot-v1-128k-vision-preview": true, "kimi-k2.7-code-highspeed": true,

	// The two audio rows genuinely refuse, and saying so took a second probe.
	// The first asked with a plain text body and got 400 "This model requires
	// that either input content or output modality contains audio" — a verdict
	// about MODALITY that says nothing about response_format. Re-asked with
	// audio output declared, both answer 400 "'response_format' of type
	// 'json_schema' is not supported with this model", and the same request
	// minus the schema answers 200. The control is what makes this an answer.
	"gpt-audio-1.5": false, "gpt-audio-mini": false,
}

func TestCatalog_StructuredOutputsMatchesWhatTheWireDid(t *testing.T) {
	rows := loadShippedCatalog(t)

	declared := map[string]bool{}
	for _, r := range rows {
		if _, probed := structuredOutputProbe[r.ModelCode]; !probed {
			continue
		}
		declared[r.ModelCode] = containsFeature(r.Features, featureStructuredOutputs)
	}

	var wrongOn, wrongOff, absent []string
	for code, honours := range structuredOutputProbe {
		got, present := declared[code]
		if !present {
			absent = append(absent, code)
			continue
		}
		switch {
		case honours && !got:
			wrongOff = append(wrongOff, code)
		case !honours && got:
			wrongOn = append(wrongOn, code)
		}
	}
	sort.Strings(wrongOn)
	sort.Strings(wrongOff)
	sort.Strings(absent)

	if len(wrongOff) > 0 {
		t.Errorf("these models serve a json_schema request but the catalog does not say so, "+
			"so `auto` will refuse to route structured output to them: %v", wrongOff)
	}
	if len(wrongOn) > 0 {
		t.Errorf("the catalog claims %v honour a json_schema, and the wire says otherwise. "+
			"kimi-k2.5 is the dangerous one: it answers HTTP 200 with prose, so a caller's "+
			"parse is the first thing that notices", wrongOn)
	}
	// The join itself has to be asserted, or this sweep can sweep NOTHING and
	// still pass. Proved by mutation: change how loadShippedCatalog builds
	// ModelCode and both tests in this file go green having compared zero rows,
	// while claiming to be "the load-bearing half" of the dimension. The loader
	// is not hypothetical drift either — it declares adapterType at the provider
	// top level, but model-catalog.json keeps it under `template`, so every
	// row's ProviderID is already "" today. ModelCode happening to line up is
	// the only reason this file works at all.
	if len(declared) == 0 {
		t.Fatalf("the probe joined 0 of %d codes — loadShippedCatalog has drifted from the "+
			"catalog file and this test is asserting nothing", len(structuredOutputProbe))
	}
	if len(absent) > 0 {
		t.Errorf("probed but missing from the shipped catalog: %v — either the row was removed "+
			"(then remove it here too, deliberately) or the join broke. Silence here used to be "+
			"a t.Logf nobody read", absent)
	}
}

// The point of the dimension, asserted end to end over the REAL catalog rather
// than a two-row fixture: a structured-output request must not be able to land
// on the one model that answers it with prose.
func TestCatalog_StructuredOutputRequestCannotLandOnASilentIgnorer(t *testing.T) {
	rows := loadShippedCatalog(t)
	kept, _, skipped := filterByCapability(rows, false, false, true, reqText, modsOf(reqText))
	if len(skipped) != 0 {
		t.Fatalf("the dimension yielded (%v) — with the catalog populated it should not have to, "+
			"and a yielded dimension silently restores the whole pool", skipped)
	}
	for _, r := range kept {
		if honours, probed := structuredOutputProbe[r.ModelCode]; probed && !honours {
			t.Errorf("%s survived a structured-output request and the wire does not honour one",
				r.ModelCode)
		}
	}
	if len(kept) == 0 {
		t.Fatal("no candidate survived — the pool cannot be empty on a request this common")
	}
	var examined int
	for _, r := range kept {
		if _, probed := structuredOutputProbe[r.ModelCode]; probed {
			examined++
		}
	}
	if examined == 0 {
		t.Fatalf("checked %d surviving rows and recognised none of them — the join is broken, "+
			"so this assertion proves nothing", len(kept))
	}
}

// probedAdapters names the wires this sweep could actually reach: a provider
// key we hold, so a question could be ASKED of it. Every other adapter in the
// catalog ships as a template for a provider we do not run, and tagging its
// rows from a sibling model's behaviour would be the inference this whole
// dimension exists to avoid.
//
// It also bounds what the guard below can claim. Outside these six, an untagged
// row means "nobody could ask", and the filter's inability to tell that apart
// from "asked, and it refuses" is a recorded product gap, not something a test
// can fix by turning red forever.
var probedAdapters = map[string]bool{
	"openai": true, "anthropic": true, "gemini": true,
	"cohere": true, "moonshot": true, "deepseek": true,
}

// catalogRowVerdict is the raw catalog view this guard needs and
// core.SmartModelRow does not carry: the adapter that serves the row, and
// whether the deployment serves it at all. A disabled row cannot be routed to,
// so leaving it unmeasured costs nothing; an enabled one cannot be left
// unmeasured without silently losing it.
type catalogRowVerdict struct {
	Code     string
	Adapter  string
	Features []string
	Enabled  bool
}

func loadCatalogRowVerdicts(t *testing.T) []catalogRowVerdict {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "..", "tools", "db-migrate", "model-catalog.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shipped catalog: %v (looked at %s)", err, path)
	}
	var cf struct {
		Providers []struct {
			// adapterType sits under `template`, not at the provider top level.
			// catalog_acceptance_test.go's loader reads it from the top level
			// and therefore gets "" for every row — see the join-count guard in
			// TestCatalog_StructuredOutputsMatchesWhatTheWireDid. This one reads
			// where the file actually keeps it, and the guard below fails if it
			// ever stops finding it.
			Template struct {
				AdapterType string `json:"adapterType"`
			} `json:"template"`
			Models []struct {
				Code     string   `json:"code"`
				Type     string   `json:"type"`
				Features []string `json:"features"`
				Seed     struct {
					Enabled *bool `json:"enabled"`
				} `json:"seed"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &cf); err != nil {
		t.Fatalf("parse shipped catalog: %v", err)
	}
	var out []catalogRowVerdict
	for _, p := range cf.Providers {
		for _, m := range p.Models {
			if m.Type != "chat" {
				continue
			}
			// A row with no explicit seed.enabled is served: that is the
			// generator's own default, and reading absence as "disabled" here
			// would excuse exactly the rows nobody has looked at.
			enabled := m.Seed.Enabled == nil || *m.Seed.Enabled
			out = append(out, catalogRowVerdict{
				Code: m.Code, Adapter: p.Template.AdapterType,
				Features: m.Features, Enabled: enabled,
			})
		}
	}
	return out
}

// The guard that stops this gap from reopening on the wires we can reach.
//
// The filter's row-level fail-open rescues a row that declares NO features —
// undescribed is not incapable. It cannot rescue a row that declares
// `function_calling, streaming` and nothing else, because that row looks
// described. So on a probed adapter, "we asked and it refuses" and "we never
// got round to asking" produce the identical outcome — excluded — and only one
// of those is a decision.
//
// That is not hypothetical. The first sweep probed the model codes the replay
// corpus happened to name and left six enabled rows behind on adapters it had
// keys for. Four of them (the three moonshot vision-previews and
// kimi-k2.7-code-highspeed) honour a json_schema perfectly and were being
// dropped from every structured-output request; the other two really do refuse,
// and saying so took a second probe with a control. Nothing was red, because
// nothing was asking.
func TestCatalog_EveryRoutableRowOnAProbedAdapterHasAVerdict(t *testing.T) {
	rows := loadCatalogRowVerdicts(t)
	if len(rows) == 0 {
		t.Fatal("the catalog yielded no chat rows — the loader has drifted and this " +
			"guard is asserting nothing")
	}

	var unmeasured []string
	var examined int
	seenAdapters := map[string]bool{}
	for _, r := range rows {
		seenAdapters[r.Adapter] = true
		if !r.Enabled || len(r.Features) == 0 || !probedAdapters[r.Adapter] {
			continue
		}
		examined++
		if containsFeature(r.Features, featureStructuredOutputs) {
			continue
		}
		if _, probed := structuredOutputProbe[r.Code]; !probed {
			unmeasured = append(unmeasured, r.Code)
		}
	}
	// Both halves of the join are asserted, because either one silently
	// emptying turns this into a test that passes by looking at nothing. The
	// adapter half is the fragile one: it is read from `template.adapterType`,
	// a nesting the sibling loader in this package already gets wrong.
	for name := range probedAdapters {
		if !seenAdapters[name] {
			t.Errorf("adapter %q is in probedAdapters but no catalog row reports it — "+
				"either the provider was removed or template.adapterType stopped parsing, "+
				"and the second one makes this guard blind", name)
		}
	}
	if examined == 0 {
		t.Fatal("no enabled chat row on a probed adapter declares any feature — the " +
			"catalog changed shape and this guard is asserting nothing")
	}

	sort.Strings(unmeasured)
	if len(unmeasured) > 0 {
		t.Errorf("these enabled rows sit on an adapter we hold a key for, declare features "+
			"but not structured_outputs, and nobody has probed them: %v\n"+
			"  the filter drops them from every json_schema request — the row-level "+
			"fail-open only rescues rows declaring NOTHING.\n"+
			"  probe each against its provider's own wire, then either tag the row or add "+
			"it to structuredOutputProbe as false. Do not guess: round 2 of this same "+
			"sweep found 4 of 6 unmeasured rows fully capable", unmeasured)
	}
}
