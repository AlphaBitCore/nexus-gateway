// Package conformance hosts the normalize conformance corpus: golden
// tests that pin the exact NormalizedPayload the production registry
// chain emits for captured wire bodies. Each case under corpus/ holds
// the raw bytes (`wire`), the adapter context (`meta.json`) and the
// golden output (`expected.json`); TestConformanceCorpus re-normalizes
// every case and fails on any drift, so decoder refactors can prove
// byte-for-byte output stability against real traffic shapes.
package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize"
	core "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// corpusRegistry returns the registry under conformance test. It is the
// exact production assembly: normalize.BuildRegistry wires Tier 1 AI
// builtins + Tier 1 per-host adapter normalizers + Tier 1.5 sniffers +
// Tier 2 pattern probe + Tier 3 verbatim fallback and freezes the
// result — every data-plane service (including nexus-hub's
// InitNormalizeRegistry) calls the same builder. Corpus results
// therefore match what production emits for the same bytes and meta.
func corpusRegistry() *core.Registry {
	return normalize.BuildRegistry()
}

// caseMeta is the on-disk shape of a corpus case's meta.json. core.Meta
// carries no JSON tags, so the field mapping is explicit here and must
// stay in lockstep with core.Meta.
type caseMeta struct {
	AdapterType  string `json:"adapterType"`
	Model        string `json:"model"`
	ContentType  string `json:"contentType"`
	Direction    string `json:"direction"`
	EndpointPath string `json:"endpointPath"`
	Stream       bool   `json:"stream"`
}

// parseMeta decodes a meta.json document into core.Meta. Unknown fields
// are rejected so a misspelled key (e.g. "adapter" for "adapterType")
// fails loudly instead of silently normalizing with an empty Meta and
// pinning the wrong decode path; note encoding/json matches field names
// case-insensitively, so pure case typos still map correctly. Direction
// is validated for the same reason: the registry candidate-key walk and
// every AI codec branch on it.
//
// The decoded meta is canonicalized exactly like the production entry
// point (core.BuildAuditFn): AdapterType is lowercased and Content-Type
// parameters (e.g. "; charset=utf-8") are stripped, so a corpus case can
// never exercise a meta shape the registry would not see in production.
func parseMeta(data []byte) (core.Meta, error) {
	var cm caseMeta
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cm); err != nil {
		return core.Meta{}, fmt.Errorf("conformance: decode meta.json: %w", err)
	}
	switch core.Direction(cm.Direction) {
	case core.DirectionRequest, core.DirectionResponse:
	default:
		return core.Meta{}, fmt.Errorf("conformance: meta.json direction must be %q or %q, got %q",
			core.DirectionRequest, core.DirectionResponse, cm.Direction)
	}
	return core.Meta{
		AdapterType:  strings.ToLower(cm.AdapterType),
		Model:        cm.Model,
		ContentType:  core.StripContentTypeParams(cm.ContentType),
		Direction:    core.Direction(cm.Direction),
		EndpointPath: cm.EndpointPath,
		Stream:       cm.Stream,
	}, nil
}

// readMeta reads and parses a case's meta.json, failing the test on any
// error so a broken case never silently passes.
func readMeta(t testing.TB, path string) core.Meta {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("conformance: read meta.json: %v", err)
		return core.Meta{}
	}
	meta, err := parseMeta(data)
	if err != nil {
		t.Fatalf("conformance: %s: %v", path, err)
		return core.Meta{}
	}
	return meta
}

// canonicalJSON renders a NormalizedPayload in the corpus golden form:
// json.MarshalIndent with two-space indent plus a trailing newline. Both
// the freshly normalized payload and the on-disk golden (after an
// unmarshal round-trip through NormalizedPayload, see readGolden) pass
// through this function, so comparison is insensitive to key order and
// whitespace in the golden file.
func canonicalJSON(payload core.NormalizedPayload) (string, error) {
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("conformance: marshal payload: %w", err)
	}
	return string(b) + "\n", nil
}

// writeGolden writes the canonical JSON form of payload to path. Used by
// the -update-golden flow.
func writeGolden(path string, payload core.NormalizedPayload) error {
	s, err := canonicalJSON(payload)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		return fmt.Errorf("conformance: write golden %s: %w", path, err)
	}
	return nil
}

// readGolden loads expected.json and re-canonicalizes it through
// NormalizedPayload so formatting drift in the file cannot cause a
// false mismatch. Unknown fields are rejected: a golden carrying a key
// NormalizedPayload does not define (a typo, or a field removed from
// the schema) would otherwise be silently dropped by the round-trip and
// the comparison would pass against a golden that no longer says what
// it appears to say.
func readGolden(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("conformance: read golden: %w", err)
	}
	var payload core.NormalizedPayload
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return "", fmt.Errorf("conformance: parse golden %s: %w", path, err)
	}
	return canonicalJSON(payload)
}

// diffLines produces a readable line-by-line diff between want and got
// for failure output: unchanged lines are prefixed with two spaces,
// golden-only lines with "- ", actual-only lines with "+ ". Positional
// compare only — enough to spot which golden field drifted.
func diffLines(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")
	n := len(wantLines)
	if len(gotLines) > n {
		n = len(gotLines)
	}
	var b strings.Builder
	for i := range n {
		var w, g string
		wOK, gOK := i < len(wantLines), i < len(gotLines)
		if wOK {
			w = wantLines[i]
		}
		if gOK {
			g = gotLines[i]
		}
		if wOK && gOK && w == g {
			b.WriteString("  " + w + "\n")
			continue
		}
		if wOK {
			b.WriteString("- " + w + "\n")
		}
		if gOK {
			b.WriteString("+ " + g + "\n")
		}
	}
	return b.String()
}
