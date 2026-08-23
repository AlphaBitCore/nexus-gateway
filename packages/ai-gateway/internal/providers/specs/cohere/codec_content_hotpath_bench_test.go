package cohere

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

// Absolute-cost measurement for translateContentForCohereChat, which runs on every
// request this adapter encodes.
//
// The claim is that a request with no attachment pays one substring search. The
// no-file arm measures that; the doc arm measures what an attachment actually
// costs, so "rare and expensive" is a statement with both halves known.
//
// A second no-file arm exists because the gate token is the QUOTED `"file"`,
// which an ordinary request can carry without carrying a file part: a tool
// schema with a `file` property or a `"required":["file"]` entry spells exactly
// those bytes. Such a body trips the gate and pays a `messages` extraction it
// throws away, which is the same wasted-parse class as the internal-carrier
// decoy, and it is measured rather than assumed.
//
// Prose, by contrast, CANNOT trip it: inside a JSON string the quotes are
// escaped as `\"`, so the word arrives as `\"file\"` and the six-byte token
// never appears. Established by the guard test below — the first version of
// this benchmark used prose and measured the cheap path under an alarming name.

var (
	cohereSinkBody []byte
	cohereSinkErr  error
)

// cohereChatBody builds an ordinary chat request with roughly promptBytes of
// user text and no attachment. tokenDecoy adds a tool schema that spells the
// gate token as JSON without carrying a file part.
func cohereChatBody(promptBytes int, tokenDecoy bool) []byte {
	filler := strings.Repeat("please review the deployment plan carefully. ", promptBytes/44+1)
	var b strings.Builder
	b.WriteString(`{"model":"command-a","messages":[{"role":"system","content":[{"type":"text",` +
		`"text":"You are a precise assistant."}]},{"role":"user","content":[{"type":"text","text":"`)
	b.WriteString(filler)
	b.WriteString(`"}]}]`)
	if tokenDecoy {
		// A caller's own tool: the argument is NAMED file, so the six-byte token
		// is present as JSON while no content part is a file part.
		b.WriteString(`,"tools":[{"type":"function","function":{"name":"read_doc","parameters":` +
			`{"type":"object","properties":{"file":{"type":"string"}},"required":["file"]}}}]`)
	}
	b.WriteString(`,"max_tokens":1024}`)
	return []byte(b.String())
}

// cohereFileBody carries a text/markdown document of docBytes alongside a
// question, which is the shape Cohere needs (a file with no text is refused).
func cohereFileBody(docBytes int) []byte {
	doc := strings.Repeat("# Runbook\n\nStep: rotate the credential and verify.\n\n", docBytes/52+1)
	b64 := base64.StdEncoding.EncodeToString([]byte(doc)[:docBytes])
	var b strings.Builder
	b.WriteString(`{"model":"command-a","messages":[{"role":"user","content":[`)
	b.WriteString(`{"type":"text","text":"What is the reference number in this runbook?"},`)
	b.WriteString(`{"type":"file","file":{"filename":"runbook.md","file_data":` +
		`"data:text/markdown;base64,`)
	b.WriteString(b64)
	b.WriteString(`"}}]}],"max_tokens":1024}`)
	return []byte(b.String())
}

func benchLift(b *testing.B, body []byte, wantLifted bool) {
	out, err := translateContentForCohereChat(body)
	if err != nil {
		b.Fatalf("arm precondition: %v", err)
	}
	if lifted := !bytes.Equal(out, body); lifted != wantLifted {
		b.Fatalf("arm precondition: lifted = %v, want %v — the benchmark is on the wrong branch",
			lifted, wantLifted)
	}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		cohereSinkBody, cohereSinkErr = translateContentForCohereChat(body)
	}
}

func BenchmarkLiftFileParts_NoFile_2KB(b *testing.B) {
	benchLift(b, cohereChatBody(2<<10, false), false)
}
func BenchmarkLiftFileParts_NoFile_200KB(b *testing.B) {
	benchLift(b, cohereChatBody(200<<10, false), false)
}

// Gate hits on a caller's tool argument named `file`; the extraction is wasted.
func BenchmarkLiftFileParts_TokenDecoy_2KB(b *testing.B) {
	benchLift(b, cohereChatBody(2<<10, true), false)
}
func BenchmarkLiftFileParts_TokenDecoy_200KB(b *testing.B) {
	benchLift(b, cohereChatBody(200<<10, true), false)
}

func BenchmarkLiftFileParts_Doc100KB(b *testing.B) {
	benchLift(b, cohereFileBody(100<<10), true)
}

// The decoy arm must genuinely trip the gate, and the no-file arm must not.
func TestLiftBenchArmsTakeTheIntendedBranches(t *testing.T) {
	if bytes.Contains(cohereChatBody(2<<10, false), fileTypeToken) {
		t.Error("the plain arm trips the gate; it is not the one-substring-search path")
	}
	if !bytes.Contains(cohereChatBody(2<<10, true), fileTypeToken) {
		t.Error("the decoy arm does not trip the gate; its ns/op describes the cheap path")
	}
	// Prose cannot reach the gate, because JSON escapes the quotes.
	prose := []byte(`{"messages":[{"role":"user","content":[{"type":"text",` +
		`"text":"unable to open \"file\" on disk"}]}]}`)
	if bytes.Contains(prose, fileTypeToken) {
		t.Error("escaped prose reached the gate; the decoy could have been built from text")
	}
	out, err := translateContentForCohereChat(cohereFileBody(4 << 10))
	if err != nil {
		t.Fatalf("the document arm errored: %v", err)
	}
	if !bytes.Contains(out, []byte(`"documents"`)) {
		t.Error("the document arm did not lift anything; it is not measuring the lift")
	}
}
