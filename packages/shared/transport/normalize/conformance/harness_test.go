package conformance

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

var updateGolden = flag.Bool("update-golden", false,
	"rewrite each corpus case's expected.json from current registry output")

// TestConformanceCorpus normalizes every case under corpus/ through the
// production registry assembly and compares the result against the
// case's golden expected.json in canonical JSON form. Run with
// -update-golden to regenerate goldens after an intentional behavior
// change.
func TestConformanceCorpus(t *testing.T) {
	caseDirs := listCaseDirs(t)
	if len(caseDirs) == 0 {
		t.Fatal("no corpus cases found under corpus/ — at least one case directory is required")
	}
	reg := corpusRegistry()
	for _, dir := range caseDirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, "wire"))
			if err != nil {
				t.Fatalf("read wire bytes: %v", err)
			}
			meta := readMeta(t, filepath.Join(dir, "meta.json"))
			logBaselineWrong(t, dir)

			got, err := reg.Normalize(context.Background(), raw, meta)
			if err != nil {
				t.Fatalf("registry.Normalize: %v", err)
			}
			goldenPath := filepath.Join(dir, "expected.json")
			if *updateGolden {
				if err := writeGolden(goldenPath, got); err != nil {
					t.Fatalf("update golden: %v", err)
				}
				t.Logf("golden rewritten: %s", goldenPath)
				return
			}
			want, err := readGolden(goldenPath)
			if err != nil {
				t.Fatalf("%v (generate it with: go test ./transport/normalize/conformance/ -update-golden)", err)
			}
			gotJSON, err := canonicalJSON(got)
			if err != nil {
				t.Fatalf("canonicalize output: %v", err)
			}
			if gotJSON != want {
				t.Errorf("normalized payload drifted from golden %s:\n%s", goldenPath, diffLines(want, gotJSON))
			}
		})
	}
}

// listCaseDirs returns every case directory under corpus/, skipping
// plain files such as the corpus README.
func listCaseDirs(t testing.TB) []string {
	t.Helper()
	entries, err := os.ReadDir("corpus")
	if err != nil {
		t.Fatalf("read corpus dir: %v", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join("corpus", e.Name()))
		}
	}
	return dirs
}

// logBaselineWrong surfaces the BASELINE-WRONG.md annotation of a case
// whose golden intentionally pins known-bad current behavior, so the
// caveat is visible in -v output next to the case result.
func logBaselineWrong(t *testing.T, dir string) {
	t.Helper()
	note, err := os.ReadFile(filepath.Join(dir, "BASELINE-WRONG.md"))
	if err != nil {
		return // no annotation — the golden is believed correct
	}
	firstLine, _, _ := strings.Cut(strings.TrimSpace(string(note)), "\n")
	t.Logf("golden pins known-bad behavior: %s", firstLine)
}

// TestSeedGoldenBusinessShape pins the business-level claims of the
// seed case beyond byte equality: a minimal OpenAI chat-completion
// response must normalize to kind=ai-chat with the assistant message,
// usage counters and finish reason populated through the FULL registry
// walk (adapterType + endpointPath candidate-key resolution), not just
// the codec in isolation.
func TestSeedGoldenBusinessShape(t *testing.T) {
	dir := filepath.Join("corpus", "openai-chat-nonstream-basic")
	raw, err := os.ReadFile(filepath.Join(dir, "wire"))
	if err != nil {
		t.Fatalf("read seed wire bytes: %v", err)
	}
	meta := readMeta(t, filepath.Join(dir, "meta.json"))
	got, err := corpusRegistry().Normalize(context.Background(), raw, meta)
	if err != nil {
		t.Fatalf("registry.Normalize: %v", err)
	}
	if got.Kind != core.KindAIChat {
		t.Errorf("kind = %q, want %q", got.Kind, core.KindAIChat)
	}
	if got.Model != "gpt-4o" {
		t.Errorf("model = %q, want %q", got.Model, "gpt-4o")
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages = %d, want 1 (%+v)", len(got.Messages), got.Messages)
	}
	msg := got.Messages[0]
	if msg.Role != core.RoleAssistant || len(msg.Content) != 1 || msg.Content[0].Text != "Hello!" {
		t.Errorf("assistant message wrong: %+v", msg)
	}
	if got.FinishReason != "stop" {
		t.Errorf("finishReason = %q, want %q", got.FinishReason, "stop")
	}
	if got.Usage == nil {
		t.Fatal("usage missing from normalized response")
	}
	if got.Usage.PromptTokens == nil || *got.Usage.PromptTokens != 10 {
		t.Errorf("usage.promptTokens = %v, want 10", got.Usage.PromptTokens)
	}
	if got.Usage.CompletionTokens == nil || *got.Usage.CompletionTokens != 2 {
		t.Errorf("usage.completionTokens = %v, want 2", got.Usage.CompletionTokens)
	}
	if got.Usage.TotalTokens == nil || *got.Usage.TotalTokens != 12 {
		t.Errorf("usage.totalTokens = %v, want 12", got.Usage.TotalTokens)
	}
}

func TestParseMetaMapsAllFields(t *testing.T) {
	meta, err := parseMeta([]byte(`{
		"adapterType": "openai",
		"model": "gpt-4o",
		"contentType": "application/json",
		"direction": "response",
		"endpointPath": "/v1/chat/completions",
		"stream": true
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := core.Meta{
		AdapterType:  "openai",
		Model:        "gpt-4o",
		ContentType:  "application/json",
		Direction:    core.DirectionResponse,
		EndpointPath: "/v1/chat/completions",
		Stream:       true,
	}
	if meta != want {
		t.Errorf("parseMeta = %+v, want %+v", meta, want)
	}
}

// TestParseMetaCanonicalizesLikeProduction pins the production-parity
// canonicalization (core.BuildAuditFn): a mixed-case adapterType and a
// parameterized Content-Type in meta.json must reach the registry in
// the exact shape the audit entry point would hand it — otherwise a
// corpus case could pin a decode path production never executes.
func TestParseMetaCanonicalizesLikeProduction(t *testing.T) {
	meta, err := parseMeta([]byte(`{
		"adapterType": "Anthropic",
		"contentType": "application/json; charset=utf-8",
		"direction": "response"
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.AdapterType != "anthropic" {
		t.Errorf("adapterType not lowercased: %q", meta.AdapterType)
	}
	if meta.ContentType != "application/json" {
		t.Errorf("contentType params not stripped: %q", meta.ContentType)
	}
}

func TestParseMetaRejectsBadDirection(t *testing.T) {
	_, err := parseMeta([]byte(`{"adapterType":"openai","direction":"sideways"}`))
	if err == nil || !strings.Contains(err.Error(), "direction") {
		t.Fatalf("want direction validation error, got %v", err)
	}
}

func TestParseMetaRejectsUnknownField(t *testing.T) {
	// A misspelled key must fail loudly, never normalize with empty
	// Meta. (Pure case typos like "adaptertype" are matched
	// case-insensitively by encoding/json and map correctly.)
	_, err := parseMeta([]byte(`{"adapter":"openai","direction":"response"}`))
	if err == nil {
		t.Fatal("want unknown-field error for misspelled key, got nil")
	}
}

func TestParseMetaRejectsMalformedJSON(t *testing.T) {
	_, err := parseMeta([]byte(`not json`))
	if err == nil {
		t.Fatal("want decode error on malformed meta.json, got nil")
	}
}

// fatalTB captures Fatalf from helpers under test. Fatalf panics with a
// sentinel so the helper stops exactly where the real testing.T would
// (runtime.Goexit semantics) and the test recovers.
type fatalTB struct {
	testing.TB
	msg string
}

type fatalSentinel struct{}

func (f *fatalTB) Helper() {}

func (f *fatalTB) Fatalf(format string, args ...any) {
	f.msg = fmt.Sprintf(format, args...)
	panic(fatalSentinel{})
}

func TestReadMetaMissingFileFailsCase(t *testing.T) {
	f := &fatalTB{TB: t}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("want readMeta to fail the case on missing meta.json")
		}
		if !strings.Contains(f.msg, "meta.json") {
			t.Errorf("fatal message should name meta.json, got %q", f.msg)
		}
	}()
	readMeta(f, filepath.Join(t.TempDir(), "meta.json"))
}

func TestReadMetaInvalidContentFailsCase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.json")
	if err := os.WriteFile(path, []byte(`{"direction":"nowhere"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fatalTB{TB: t}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("want readMeta to fail the case on invalid direction")
		}
		if !strings.Contains(f.msg, "direction") {
			t.Errorf("fatal message should name the direction error, got %q", f.msg)
		}
	}()
	readMeta(f, path)
}

// TestWriteGoldenReadGoldenRoundTrip verifies the -update-golden write
// path produces exactly what the compare path reads back: canonical
// indent, trailing newline, formatting-insensitive reload.
func TestWriteGoldenReadGoldenRoundTrip(t *testing.T) {
	ten := 10
	payload := core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Model:            "gpt-4o",
		Usage:            &core.Usage{PromptTokens: &ten},
	}
	path := filepath.Join(t.TempDir(), "expected.json")
	if err := writeGolden(path, payload); err != nil {
		t.Fatalf("writeGolden: %v", err)
	}
	got, err := readGolden(path)
	if err != nil {
		t.Fatalf("readGolden: %v", err)
	}
	want, err := canonicalJSON(payload)
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	if got != want {
		t.Errorf("round-trip mismatch:\n%s", diffLines(want, got))
	}
	if !strings.HasSuffix(got, "\n") {
		t.Error("golden must end with a trailing newline")
	}
}

// TestReadGoldenCanonicalizesFormatting proves a golden saved compact /
// key-reordered still compares equal — the harness compares canonical
// forms, not file bytes.
func TestReadGoldenCanonicalizesFormatting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "expected.json")
	compact := `{"model":"gpt-4o","kind":"ai-chat","normalizeVersion":"1"}`
	if err := os.WriteFile(path, []byte(compact), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readGolden(path)
	if err != nil {
		t.Fatalf("readGolden: %v", err)
	}
	want, err := canonicalJSON(core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: "1",
		Model:            "gpt-4o",
	})
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	if got != want {
		t.Errorf("compact golden did not canonicalize:\n%s", diffLines(want, got))
	}
}

func TestReadGoldenErrors(t *testing.T) {
	if _, err := readGolden(filepath.Join(t.TempDir(), "expected.json")); err == nil {
		t.Error("want error for missing golden file")
	}
	bad := filepath.Join(t.TempDir(), "expected.json")
	if err := os.WriteFile(bad, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readGolden(bad); err == nil {
		t.Error("want error for unparseable golden file")
	}
}

// TestReadGoldenRejectsUnknownField: a golden carrying a key that
// NormalizedPayload does not define (typo, or a field removed from the
// schema) must fail loudly — the unmarshal round-trip would otherwise
// silently drop it and the comparison would pass against a golden that
// no longer says what it appears to say.
func TestReadGoldenRejectsUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "expected.json")
	stale := `{"kind":"ai-chat","normalizeVersion":"1","finishreason_typo":"stop"}`
	if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readGolden(path); err == nil {
		t.Error("want unknown-field error for golden with undefined key")
	}
}

// TestCanonicalJSONMarshalFailure exercises the marshal error branch via
// a payload carrying an unmarshalable tool schema value.
func TestCanonicalJSONMarshalFailure(t *testing.T) {
	payload := core.NormalizedPayload{
		Kind: core.KindAIChat,
		Tools: []core.ToolDef{{
			Name:                 "broken",
			ParametersJSONSchema: map[string]any{"ch": make(chan int)},
		}},
	}
	if _, err := canonicalJSON(payload); err == nil {
		t.Error("want marshal error for unmarshalable payload")
	}
	if err := writeGolden(filepath.Join(t.TempDir(), "expected.json"), payload); err == nil {
		t.Error("want writeGolden to propagate the marshal error")
	}
}

func TestDiffLinesMarksChanges(t *testing.T) {
	want := "a\nb\nc\n"
	got := "a\nB\nc\nd\n"
	diff := diffLines(want, got)
	for _, expect := range []string{"  a\n", "- b\n", "+ B\n", "  c\n", "+ d\n"} {
		if !strings.Contains(diff, expect) {
			t.Errorf("diff missing %q:\n%s", expect, diff)
		}
	}
}
