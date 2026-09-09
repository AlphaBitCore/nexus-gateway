// Package canonicalbridge — the request fixtures answer to the model catalog.
//
// Named failure modes:
//   - a fixture carries a media part the target's models do not accept, so the
//     test measures a rejection instead of the conversion
//   - a fixture omits a media part the target DOES accept, so the gate passes
//     without ever exercising it
package canonicalbridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

// catalogPath is the repo's declaration of what each provider's chat models
// accept. It is the reason this file exists: which media a request fixture may
// carry is a fact the repo already states, and picking media by hand instead
// produced both errors this guards against — a Cohere fixture carrying an https
// image (which that wire refuses) written from memory, and no fixture at all
// for the file part Cohere does accept.
const catalogPath = "../../../../../tools/db-migrate/model-catalog.json"

// catalogKey maps a canonical Format to its provider key in the catalog.
var catalogKey = map[provcore.Format]string{
	provcore.FormatOpenAI:      "openai",
	provcore.FormatAnthropic:   "anthropic",
	provcore.FormatGemini:      "google-gemini",
	provcore.FormatCohere:      "cohere",
	provcore.FormatAzureOpenAI: "azure-openai",
	provcore.FormatDeepSeek:    "deepseek",
	provcore.FormatMoonshot:    "moonshot",
	provcore.FormatVertex:      "vertex",
}

// chatInputModalities reads the UNION of input modalities across a provider's
// chat models — which is the right answer for "may a fixture carry this part
// at all" and the wrong one for "may THIS model take it".
//
// The distinction is not academic. Cohere declares image on exactly one chat
// model, and building a capture against the union sent an image to
// command-a-03-2025, which answered `400 image content is not supported for
// this model`. A capture picks a model and must read that model's row; a
// fixture check asks whether the wire has anywhere to put the part, and the
// union answers that.
func chatInputModalities(t *testing.T, format provcore.Format) map[string]bool {
	t.Helper()
	key, ok := catalogKey[format]
	if !ok {
		t.Fatalf("no catalog key mapped for %s — add one rather than skipping, or the fixture "+
			"for this format answers to nothing", format)
	}
	raw, err := os.ReadFile(filepath.Clean(catalogPath))
	if err != nil {
		t.Fatalf("read model catalog: %v", err)
	}
	var catalog struct {
		Providers []struct {
			Key    string `json:"key"`
			Models []struct {
				Type            string   `json:"type"`
				InputModalities []string `json:"inputModalities"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatalf("parse model catalog: %v", err)
	}
	out := map[string]bool{}
	for _, p := range catalog.Providers {
		if p.Key != key {
			continue
		}
		for _, m := range p.Models {
			if m.Type != "chat" {
				continue
			}
			for _, mod := range m.InputModalities {
				out[mod] = true
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("the catalog declares no chat input modalities for %q — either the key is wrong "+
			"or the catalog changed shape; a fixture check against an empty set proves nothing", key)
	}
	return out
}

// TestRequestFixturesMatchTheCatalog ties the shared rich fixtures to the
// catalog in both directions: nothing they carry may be undeclared, and nothing
// the catalog declares as media may go untested without being named here.
func TestRequestFixturesMatchTheCatalog(t *testing.T) {
	for _, shape := range []provcore.Format{
		provcore.FormatOpenAI,
		provcore.FormatAnthropic,
		provcore.FormatGemini,
	} {
		t.Run(string(shape), func(t *testing.T) {
			declared := chatInputModalities(t, shape)
			carried := fixtureModalities(richNativeChatBody(shape))

			for mod := range carried {
				if !declared[mod] {
					t.Errorf("the %s fixture carries a %q part, which no %s chat model declares — "+
						"the codec will refuse it and the test will be measuring a rejection",
						shape, mod, shape)
				}
			}
			// The reverse gap is the quieter one: a modality the wire accepts and
			// the fixture never sends is a conversion nothing exercises. Named
			// here rather than asserted, because covering audio and video means
			// fixtures and captures this suite does not have yet.
			var missing []string
			for mod := range declared {
				if mod == "text" || carried[mod] {
					continue
				}
				missing = append(missing, mod)
			}
			sort.Strings(missing)
			if len(missing) > 0 {
				t.Logf("%s accepts %v but the fixture sends none — untested conversions", shape, missing)
			}
		})
	}
}

// fixtureModalities reads which media kinds a native chat body carries. It walks
// all three wire shapes because the fixtures are native, not canonical.
func fixtureModalities(body string) map[string]bool {
	out := map[string]bool{}
	root := gjson.Parse(body)

	// OpenAI chat: messages[].content[] with type image_url / input_audio / file.
	root.Get("messages").ForEach(func(_, m gjson.Result) bool {
		m.Get("content").ForEach(func(_, part gjson.Result) bool {
			switch part.Get("type").String() {
			case "image_url", "image":
				out["image"] = true
			case "input_audio":
				out["audio"] = true
			case "file", "document":
				out["file"] = true
			}
			return true
		})
		return true
	})

	// Gemini: contents[].parts[] with fileData / inlineData, classified by mime.
	root.Get("contents").ForEach(func(_, c gjson.Result) bool {
		c.Get("parts").ForEach(func(_, part gjson.Result) bool {
			mime := part.Get("fileData.mimeType").String()
			if mime == "" {
				mime = part.Get("inlineData.mimeType").String()
			}
			switch {
			case strings.HasPrefix(mime, "image/"):
				out["image"] = true
			case strings.HasPrefix(mime, "audio/"):
				out["audio"] = true
			case strings.HasPrefix(mime, "video/"):
				out["video"] = true
			case mime != "":
				out["file"] = true
			}
			return true
		})
		return true
	})
	return out
}
