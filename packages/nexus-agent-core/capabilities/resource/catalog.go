// Package resource is the generic, OpenAPI-driven REST engine behind the toolkit's
// resource_* agent tools and the TUI resource cascade. It embeds the generated
// Control Plane OpenAPI catalog and, from that alone, derives everything needed to
// invoke ANY admin operation — the ordered path parameters, a canonical verb where
// one applies, a compact per-operation schema, and a code-level ranked search — with
// no per-kind special-casing. It is a pure leaf: it imports only stdlib + yaml, knows
// nothing about the agent kernel or the gateway client, so the tool wiring that needs
// those lives in the parent capabilities package and consumes this engine's API.
package resource

import (
	"embed"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// resourceSpecFS embeds the generated CP OpenAPI catalog + per-kind specs so the
// generic resource tools work on a distributed CLI with no repo on disk. The copy
// under openapi/control-plane/ is kept in sync with docs/users/api/openapi/
// control-plane/ by `go generate ./internal/...` (see generate.go).
//
//go:embed openapi/control-plane/*.yaml
var resourceSpecFS embed.FS

const resourceSpecDir = "openapi/control-plane"

// resourceCatalog is the parsed _index.yaml: the resource-kind catalog that drives
// the generic tools. One entry per kind; every operation lists method/path/tier.
type resourceCatalog struct {
	BasePrefix string         `yaml:"basePrefix"`
	Kinds      []resourceKind `yaml:"kinds"`
}

type resourceKind struct {
	Kind       string       `yaml:"kind"`
	File       string       `yaml:"file"`
	Operations []resourceOp `yaml:"operations"`
}

type resourceOp struct {
	Method      string `yaml:"method"`
	Path        string `yaml:"path"`
	OperationID string `yaml:"operationId"`
	Tier        string `yaml:"tier"`
	IAMAction   string `yaml:"iamAction"`
}

// resCatalog / resIdx are the parsed embedded catalog. The embed is a build-time
// artifact, so a read/parse failure is a programmer/build error, not a runtime
// condition — init panics on it (CI/tests catch it immediately) and the engine
// treats the catalog as always-present.
var (
	resCatalog *resourceCatalog
	resIdx     map[string]resourceKind
)

func init() {
	raw, err := resourceSpecFS.ReadFile(resourceSpecDir + "/_index.yaml")
	if err != nil {
		panic("resource: embedded resource index missing: " + err.Error())
	}
	cat, idx, err := parseCatalog(raw)
	if err != nil {
		panic("resource: embedded resource index invalid: " + err.Error())
	}
	resCatalog, resIdx = cat, idx
	// Precompute the search corpus once (summary + query-param text per op) so
	// Search never re-distills the specs on a query.
	opIndex = buildOpIndex()
}

// parseCatalog parses the _index.yaml bytes into a catalog + a by-kind index. Pure
// (no embed) so the parse path is unit-testable, including malformed input.
func parseCatalog(raw []byte) (*resourceCatalog, map[string]resourceKind, error) {
	var c resourceCatalog
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, nil, fmt.Errorf("parse resource index: %w", err)
	}
	idx := make(map[string]resourceKind, len(c.Kinds))
	for _, k := range c.Kinds {
		idx[k.Kind] = k
	}
	return &c, idx, nil
}

// KindNames returns the sorted kind names (shown to the model when it asks for an
// unknown kind, so it can correct itself).
func KindNames() []string {
	out := make([]string, 0, len(resCatalog.Kinds))
	for _, k := range resCatalog.Kinds {
		out = append(out, k.Kind)
	}
	sort.Strings(out)
	return out
}

// collectionPath is the kind's collection endpoint (basePrefix + "/" + kind);
// /{id} and /{id}/<action> hang off it.
func collectionPath(kind string) string {
	return strings.TrimRight(resCatalog.BasePrefix, "/") + "/" + kind
}

// isSingleParamSeg reports whether s is exactly one "{param}" segment.
func isSingleParamSeg(s string) bool {
	return strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") && !strings.Contains(s, "/")
}
