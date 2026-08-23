package specutil_test

import (
	"bytes"
	"encoding/base64"
	"github.com/tidwall/gjson"
	"slices"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// passthrough records what reached the inner codec, so "the gate let it through"
// is asserted as the inner codec SEEING it rather than as the absence of an
// error — a gate that swallowed a request would pass the weaker check.
type passthrough struct {
	sawEncode []byte
	sawNative []byte
}

func (p *passthrough) EncodeRequest(_ typology.WireShape, body []byte,
	_ provcore.CallTarget) (provcore.EncodeResult, error) {
	p.sawEncode = body
	return provcore.EncodeResult{Body: body}, nil
}

func (p *passthrough) RewriteNative(_ typology.WireShape, body []byte,
	_ provcore.CallTarget, _ bool) (provcore.EncodeResult, error) {
	p.sawNative = body
	return provcore.EncodeResult{Body: body}, nil
}

func (p *passthrough) DecodeResponse(typology.WireShape, []byte, string,
	provcore.DecodeContext) (provcore.DecodeResult, error) {
	return provcore.DecodeResult{}, nil
}

func gated(policy specutil.ContentPolicy) (provcore.SchemaCodec, *passthrough) {
	inner := &passthrough{}
	return specutil.GateContent(inner, specutil.UniformPolicy(policy)), inner
}

var textOnly = specutil.ContentPolicy{
	Allow: map[string]bool{},
	Deny:  map[string]string{"file": "this wire has no document part"},
}

// A refused kind carries the adapter's own explanation, on BOTH doors — the
// native leg is the one an OpenAI-shaped request takes, and a gate that misses
// it changes nothing in production while passing every other test.
func TestGateContent_RefusesOnBothDoors(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[
	    {"type":"file","file":{"file_data":"data:text/plain;base64,QQ=="}},
	    {"type":"text","text":"q"}]}]}`)

	c, inner := gated(textOnly)
	if _, err := c.EncodeRequest(typology.WireShapeOpenAIChat, body, provcore.CallTarget{}); err == nil {
		t.Error("the cross-format door forwarded a refused part")
	} else if !strings.Contains(err.Error(), "no document part") {
		t.Errorf("message %q is not the adapter's own explanation", err.Error())
	}
	if _, err := c.RewriteNative(typology.WireShapeOpenAIChat, body, provcore.CallTarget{}, false); err == nil {
		t.Error("the native door forwarded a refused part — this is the path production uses")
	}
	if inner.sawEncode != nil || inner.sawNative != nil {
		t.Error("the inner codec saw a body the gate should have refused")
	}
}

// A kind nobody wrote a reason for still gets refused, because silently
// forwarding it is what the gate exists to stop — but the generic message is
// deliberately worse, which is the pressure to write a real one.
func TestGateContent_AnUnlistedKindIsStillRefused(t *testing.T) {
	c, _ := gated(textOnly)
	_, err := c.EncodeRequest(typology.WireShapeOpenAIChat,
		[]byte(`{"messages":[{"role":"user","content":[{"type":"video_url","video_url":{"url":"x"}}]}]}`),
		provcore.CallTarget{})
	if err == nil {
		t.Fatal("an unlisted content kind was forwarded")
	}
	if !strings.Contains(err.Error(), "video_url") {
		t.Errorf("message %q does not name the kind that was refused", err.Error())
	}
}

// Everything the policy allows must reach the inner codec untouched, and so must
// every body with nothing to check. The last case is the pre-scan's trap: a
// tool_calls turn carries `type` outside any content part.
func TestGateContent_AllowedAndIrrelevantBodiesReachTheCodec(t *testing.T) {
	policy := specutil.ContentPolicy{Allow: map[string]bool{"image_url": true}}
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"plain string"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"parts"}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/a.png"}}]}]}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`,
		`{"messages":[]}`,
	} {
		c, inner := gated(policy)
		if _, err := c.EncodeRequest(typology.WireShapeOpenAIChat, []byte(body), provcore.CallTarget{}); err != nil {
			t.Errorf("refused a body it should carry: %v\n  %s", err, body)
			continue
		}
		if inner.sawEncode == nil {
			t.Errorf("the inner codec never saw the body: %s", body)
		}
	}
}

// InlineOnlyImageURL is the per-wire fact that an image must arrive as bytes.
// Measured on two providers: a data: URL answers 200, any http(s) URL answers
// 400 categorically, so the URL itself is never the problem and the message must
// not suggest it is.
func TestGateContent_InlineOnlyImageURL(t *testing.T) {
	policy := specutil.ContentPolicy{
		Allow:              map[string]bool{"image_url": true},
		InlineOnlyImageURL: "this wire does not fetch images by URL",
	}
	c, _ := gated(policy)

	_, err := c.EncodeRequest(typology.WireShapeOpenAIChat,
		[]byte(`{"messages":[{"role":"user","content":[
		    {"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`),
		provcore.CallTarget{})
	if err == nil || !strings.Contains(err.Error(), "does not fetch images by URL") {
		t.Errorf("err = %v, want the constraint named", err)
	}

	if _, err := c.EncodeRequest(typology.WireShapeOpenAIChat,
		[]byte(`{"messages":[{"role":"user","content":[
		    {"type":"image_url","image_url":{"url":"data:image/png;base64,QQ=="}}]}]}`),
		provcore.CallTarget{}); err != nil {
		t.Errorf("an inline image was refused: %v", err)
	}

	// A wire that DOES fetch leaves the URL alone.
	fetching, _ := gated(specutil.ContentPolicy{Allow: map[string]bool{"image_url": true}})
	if _, err := fetching.EncodeRequest(typology.WireShapeOpenAIChat,
		[]byte(`{"messages":[{"role":"user","content":[
		    {"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`),
		provcore.CallTarget{}); err != nil {
		t.Errorf("a fetching wire refused a URL: %v", err)
	}
}

// The policy is a function of the CALL TARGET, because the divergence is per
// model and not per adapter: inside one OpenAI-compatible family a model reads
// images and its sibling does not. A gate keyed on the adapter is wrong for
// most of that adapter's models whichever way it is set — and before this, the
// gate could not see which model it was gating at all.
//
// Asserted on BOTH doors. The native leg is the one an OpenAI-shaped request
// takes, so a resolver consulted on only the cross-format door would leave
// production keyed on nothing.
func TestGateContent_PolicyIsResolvedPerTarget(t *testing.T) {
	seesImages := specutil.ContentPolicy{Allow: map[string]bool{"image_url": true}}
	blind := specutil.ContentPolicy{
		Allow: map[string]bool{},
		Deny:  map[string]string{"image_url": "this model has no vision"},
	}
	inner := &passthrough{}
	var asked []string
	c := specutil.GateContent(inner, func(tgt provcore.CallTarget) specutil.ContentPolicy {
		asked = append(asked, tgt.ProviderModelID)
		if tgt.ProviderModelID == "sees" {
			return seesImages
		}
		return blind
	})

	body := []byte(`{"messages":[{"role":"user","content":[
	    {"type":"image_url","image_url":{"url":"data:image/png;base64,QQ=="}}]}]}`)

	for _, door := range []string{"encode", "native"} {
		send := func(model string) error {
			tgt := provcore.CallTarget{ProviderModelID: model}
			if door == "encode" {
				_, err := c.EncodeRequest(typology.WireShapeOpenAIChat, body, tgt)
				return err
			}
			_, err := c.RewriteNative(typology.WireShapeOpenAIChat, body, tgt, false)
			return err
		}
		if err := send("sees"); err != nil {
			t.Errorf("%s door refused an image for a model that reads them: %v", door, err)
		}
		err := send("blind")
		if err == nil {
			t.Fatalf("%s door forwarded an image to a model with no vision — the gate is keyed "+
				"on the adapter, not the model", door)
		}
		if !strings.Contains(err.Error(), "no vision") {
			t.Errorf("message %q is not the policy chosen for THIS model", err.Error())
		}
	}
	if len(asked) == 0 {
		t.Fatal("the resolver was never consulted")
	}
	for _, m := range asked {
		if m != "sees" && m != "blind" {
			t.Errorf("the resolver saw %q, not the target under test", m)
		}
	}
}

// A body with no structured content part must not even resolve a policy — that
// is the overwhelming majority of traffic and it is not a request any policy
// has an opinion about.
func TestGateContent_PlainTextDoesNotResolveAPolicy(t *testing.T) {
	resolved := 0
	c := specutil.GateContent(&passthrough{}, func(provcore.CallTarget) specutil.ContentPolicy {
		resolved++
		return specutil.ContentPolicy{}
	})
	_, err := c.EncodeRequest(typology.WireShapeOpenAIChat,
		[]byte(`{"messages":[{"role":"user","content":"plain text"}]}`), provcore.CallTarget{})
	if err != nil {
		t.Fatalf("a text-only request was refused: %v", err)
	}
	if resolved != 0 {
		t.Errorf("resolved a policy %d times for a request carrying no content part", resolved)
	}
}

// application/octet-stream says only "unknown". What follows from that depends
// entirely on the bytes, and getting it wrong in either direction costs a
// capability:
//
//   - bytes that ARE text: relabel them. This is the goal's tier 1 — the bytes
//     are markdown and only the label is wrong. Refusing here was measured
//     taking a document Gemini 3.x had been READING and turning it into our own
//     400, on a caller whose browser chose the type for them.
//   - bytes that are not: refuse, naming what to declare instead. Relabelling
//     binary as text puts mojibake in front of the model and earns a confident
//     answer about nothing.
func TestGateContent_UndeclaredTextIsRelabelledNotRefused(t *testing.T) {
	c, inner := gated(specutil.ContentPolicy{Allow: map[string]bool{"file": true}})
	text := base64.StdEncoding.EncodeToString([]byte("# Report\n\nreference 52903\n"))
	res, err := c.EncodeRequest(typology.WireShapeOpenAIChat,
		[]byte(`{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"x",
		    "file_data":"data:application/octet-stream;base64,`+text+`"}}]}]}`),
		provcore.CallTarget{})
	if err != nil {
		t.Fatalf("an octet-stream attachment carrying text was refused: %v", err)
	}
	got := gjson.GetBytes(inner.sawEncode, "messages.0.content.0.file.file_data").String()
	if !strings.HasPrefix(got, "data:text/plain;base64,") {
		t.Errorf("file_data = %q, want it relabelled to text/plain", got)
	}
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(got, "data:text/plain;base64,"))
	if !strings.Contains(string(raw), "52903") {
		t.Errorf("the document's characters did not survive the relabel: %q", raw)
	}
	if !slices.Contains(res.Rewrites, "octet_stream_relabelled_as_text") {
		t.Errorf("the relabel was not announced: rewrites = %v", res.Rewrites)
	}
}

func TestGateContent_UndeclaredBinaryIsStillRefused(t *testing.T) {
	c, inner := gated(specutil.ContentPolicy{Allow: map[string]bool{"file": true}})
	for name, payload := range map[string][]byte{
		"pdf":      []byte("%PDF-1.4 stream"),
		"png":      {0x89, 'P', 'N', 'G', 0x0d, 0x0a},
		"nul-byte": {'h', 'i', 0x00, 'x'},
		"not-utf8": {0xff, 0xfe, 'a'},
	} {
		t.Run(name, func(t *testing.T) {
			b64 := base64.StdEncoding.EncodeToString(payload)
			_, err := c.EncodeRequest(typology.WireShapeOpenAIChat,
				[]byte(`{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"x",
				    "file_data":"data:application/octet-stream;base64,`+b64+`"}}]}]}`),
				provcore.CallTarget{})
			if err == nil {
				t.Fatal("binary bytes were relabelled as text")
			}
			if !strings.Contains(err.Error(), "octet-stream") {
				t.Errorf("message %q does not name what was declared", err.Error())
			}
			if !strings.Contains(err.Error(), "text/markdown") {
				t.Errorf("message %q does not say what to declare instead", err.Error())
			}
			if inner.sawEncode != nil {
				t.Error("the inner codec saw a body the gate should have refused")
			}
		})
	}
}

// A document that DOES declare its type is untouched by that check — the
// negative case, without which removing the octet-stream comparison passes.
func TestGateContent_ADeclaredDocumentTypeIsNotRefused(t *testing.T) {
	c, _ := gated(specutil.ContentPolicy{Allow: map[string]bool{"file": true}})
	for _, mt := range []string{"application/pdf", "text/markdown", "text/plain"} {
		if _, err := c.EncodeRequest(typology.WireShapeOpenAIChat,
			[]byte(`{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"x",
			    "file_data":"data:`+mt+`;base64,QQ=="}}]}]}`),
			provcore.CallTarget{}); err != nil {
			t.Errorf("%s was refused: %v", mt, err)
		}
	}
}

// The tier-2 conversion, at the shared level: a textual document becomes a text
// part and the conversion is reported, while a PDF is left alone.
func TestGateContent_InlinesTextDocumentsAndReportsIt(t *testing.T) {
	policy := specutil.ContentPolicy{
		Allow:               map[string]bool{"file": true},
		InlineTextDocuments: "this wire reads a document only as a PDF",
	}
	c, inner := gated(policy)
	doc := base64.StdEncoding.EncodeToString([]byte("reference 52903"))
	res, err := c.EncodeRequest(typology.WireShapeOpenAIChat,
		[]byte(`{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"d.md",
		    "file_data":"data:text/markdown;base64,`+doc+`"}}]}]}`),
		provcore.CallTarget{})
	if err != nil {
		t.Fatalf("a text document was refused rather than converted: %v", err)
	}
	if bytes.Contains(inner.sawEncode, []byte("file_data")) {
		t.Error("a file part reached the inner codec; the wire will refuse it")
	}
	if !bytes.Contains(inner.sawEncode, []byte("52903")) {
		t.Error("the document's content was lost in the conversion")
	}
	if len(res.Rewrites) == 0 {
		t.Error("the conversion is silent — a caller cannot tell it happened")
	}

	// A PDF is not characters: left alone, and no coercion reported.
	res, err = c.EncodeRequest(typology.WireShapeOpenAIChat,
		[]byte(`{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"d.pdf",
		    "file_data":"data:application/pdf;base64,JVBERi0x"}}]}]}`),
		provcore.CallTarget{})
	if err != nil {
		t.Fatalf("a PDF was refused: %v", err)
	}
	if !bytes.Contains(inner.sawEncode, []byte("file_data")) {
		t.Error("a PDF was converted to text; it is not characters")
	}
	if len(res.Rewrites) != 0 {
		t.Errorf("a coercion was reported for an untouched PDF: %v", res.Rewrites)
	}
}

// audioLead is the policy of a wire whose audio models only read the attachment
// when a text part comes first.
var audioLead = specutil.ContentPolicy{
	Allow:              map[string]bool{"text": true, "input_audio": true, "image_url": true},
	TextPartLeadsAudio: "this wire reads a lone audio attachment only when text leads",
}

func encodeThrough(t *testing.T, policy specutil.ContentPolicy, body string) ([]byte, provcore.EncodeResult) {
	t.Helper()
	codec, inner := gated(policy)
	res, err := codec.EncodeRequest(typology.WireShapeOpenAIChat, []byte(body), provcore.CallTarget{})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return inner.sawEncode, res
}

func partTypes(body []byte) []string {
	var out []string
	gjson.GetBytes(body, "messages.0.content").ForEach(func(_, p gjson.Result) bool {
		out = append(out, p.Get("type").String())
		return true
	})
	return out
}

// TestTextPartLeadsAudio_MovesTheInstructionAheadOfALoneRecording.
//
// Measured 6/6 against 0/6 on both OpenAI audio models with identical bytes:
// [text, input_audio] returns the transcript, [input_audio, text] returns HTTP
// 200 and "I'm sorry, but I can't transcribe audio directly."
//
// The 200 is why the gateway owns this. The failing form is indistinguishable
// from a model that cannot do audio — in the response and on the traffic row
// alike — and attachment-first is the natural way to compose the request.
func TestTextPartLeadsAudio_MovesTheInstructionAheadOfALoneRecording(t *testing.T) {
	got, res := encodeThrough(t, audioLead, `{"messages":[{"role":"user","content":[
		{"type":"input_audio","input_audio":{"data":"AAA=","format":"wav"}},
		{"type":"text","text":"transcribe this"}]}]}`)

	if want := []string{"text", "input_audio"}; !slices.Equal(partTypes(got), want) {
		t.Errorf("parts = %v, want %v — the request reaches the wire attachment-first, which "+
			"answers 200 with a refusal to transcribe, and nothing in the response or the "+
			"traffic row separates that from a model with no audio support",
			partTypes(got), want)
	}
	if !slices.Contains(res.Rewrites, "audio_part_moved_after_text") {
		t.Errorf("rewrites = %v — the reorder is not announced, so a caller comparing what "+
			"they sent with what was served has nothing to read", res.Rewrites)
	}
}

// TestTextPartLeadsAudio_LeavesAnOrderTheCallerMeant.
//
// The reorder is only safe where the caller expressed no opinion: one
// attachment and an instruction. Two attachments make the order the meaning —
// "compare the first recording with the second" — and moving parts changes what
// was asked. Text already leading needs nothing. No text at all leaves nothing
// to lead with.
func TestTextPartLeadsAudio_LeavesAnOrderTheCallerMeant(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{
			name: "two recordings — the order is the question",
			body: `{"messages":[{"role":"user","content":[
				{"type":"input_audio","input_audio":{"data":"AAA=","format":"wav"}},
				{"type":"input_audio","input_audio":{"data":"BBB=","format":"wav"}},
				{"type":"text","text":"compare the first with the second"}]}]}`,
			want: []string{"input_audio", "input_audio", "text"},
		},
		{
			name: "audio beside another attachment",
			body: `{"messages":[{"role":"user","content":[
				{"type":"input_audio","input_audio":{"data":"AAA=","format":"wav"}},
				{"type":"image_url","image_url":{"url":"https://ex.com/a.png"}},
				{"type":"text","text":"which one mentions the chart"}]}]}`,
			want: []string{"input_audio", "image_url", "text"},
		},
		{
			name: "text already leads",
			body: `{"messages":[{"role":"user","content":[
				{"type":"text","text":"transcribe this"},
				{"type":"input_audio","input_audio":{"data":"AAA=","format":"wav"}}]}]}`,
			want: []string{"text", "input_audio"},
		},
		{
			name: "no text to lead with",
			body: `{"messages":[{"role":"user","content":[
				{"type":"input_audio","input_audio":{"data":"AAA=","format":"wav"}}]}]}`,
			want: []string{"input_audio"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, res := encodeThrough(t, audioLead, tc.body)
			if !slices.Equal(partTypes(got), tc.want) {
				t.Errorf("parts = %v, want %v — the gate rearranged an order the caller chose",
					partTypes(got), tc.want)
			}
			if slices.Contains(res.Rewrites, "audio_part_moved_after_text") {
				t.Error("a reorder was announced although none was needed")
			}
		})
	}
}

// A wire that does not care about part order leaves every request alone, even
// the one shape the reorder exists for.
func TestTextPartLeadsAudio_IsOptOutPerWire(t *testing.T) {
	silent := specutil.ContentPolicy{Allow: map[string]bool{"text": true, "input_audio": true}}
	got, res := encodeThrough(t, silent, `{"messages":[{"role":"user","content":[
		{"type":"input_audio","input_audio":{"data":"AAA=","format":"wav"}},
		{"type":"text","text":"transcribe this"}]}]}`)

	if want := []string{"input_audio", "text"}; !slices.Equal(partTypes(got), want) {
		t.Errorf("parts = %v, want %v — a wire that never asked for the reorder got one, "+
			"which changes bytes on every audio request it serves", partTypes(got), want)
	}
	if len(res.Rewrites) != 0 {
		t.Errorf("rewrites = %v, want none", res.Rewrites)
	}
}

// TestImageFormats_RefusesADeclaredTypeThisWireDoesNotDecode.
//
// The list is the COMPLEMENT of the measured refusals, so what it refuses is
// what a wire was actually seen to reject. Owning the refusal is the point: the
// vendors' own answers name neither the file the caller sent nor anything they
// can do — "You uploaded an unsupported image", "Unsupported MIME type" — and
// five wires spell it five different ways.
func TestImageFormats_RefusesADeclaredTypeThisWireDoesNotDecode(t *testing.T) {
	policy := specutil.ContentPolicy{
		Allow:        map[string]bool{"text": true, "image_url": true},
		ImageFormats: map[string]bool{"image/png": true, "image/jpeg": true},
	}
	codec, inner := gated(policy)
	_, err := codec.EncodeRequest(typology.WireShapeOpenAIChat,
		[]byte(`{"messages":[{"role":"user","content":[
			{"type":"image_url","image_url":{"url":"data:image/heic;base64,AAAA"}}]}]}`),
		provcore.CallTarget{})

	if err == nil {
		t.Fatalf("a heic image reached the wire; the vendor answers with a message naming "+
			"neither the file nor a fix. inner saw %s", inner.sawEncode)
	}
	msg := err.Error()
	if !strings.Contains(msg, "image/heic") {
		t.Errorf("the refusal does not name what was sent: %v", msg)
	}
	if !strings.Contains(msg, "image/jpeg") || !strings.Contains(msg, "image/png") {
		t.Errorf("the refusal does not name what would work, so the caller has to guess: %v", msg)
	}
	if !strings.Contains(msg, "route to a model") {
		t.Errorf("the refusal offers no way forward: %v", msg)
	}
}

// TestImageFormats_JudgesOnlyWhatItCanSee.
//
// Three cases the list must not touch. A fetched URL carries no declared type
// here — the wire discovers it on fetch, and refusing on a filename extension
// is a guess about someone else's server. An unparseable data URL belongs to
// the codec that decodes it. An empty list means the wire's formats are unknown
// to us, and inventing refusals there takes away capability the wire has: a
// list built from formats measured to WORK would have refused image/bmp on a
// wire that reads it.
func TestImageFormats_JudgesOnlyWhatItCanSee(t *testing.T) {
	strict := specutil.ContentPolicy{
		Allow:        map[string]bool{"text": true, "image_url": true},
		ImageFormats: map[string]bool{"image/png": true},
	}
	unknown := specutil.ContentPolicy{Allow: map[string]bool{"text": true, "image_url": true}}

	for _, tc := range []struct {
		name   string
		policy specutil.ContentPolicy
		url    string
	}{
		{"a fetched URL declares nothing here", strict, "https://ex.com/photo.heic"},
		{"an unparseable data URL is the codec's business", strict, "data:not-a-media-type"},
		{"an empty list means the formats are unknown to us", unknown, "data:image/heic;base64,AAAA"},
		{"a listed format passes", strict, "data:image/png;base64,AAAA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codec, inner := gated(tc.policy)
			body := `{"messages":[{"role":"user","content":[
				{"type":"image_url","image_url":{"url":"` + tc.url + `"}}]}]}`
			if _, err := codec.EncodeRequest(typology.WireShapeOpenAIChat, []byte(body), provcore.CallTarget{}); err != nil {
				t.Fatalf("refused something the list cannot judge: %v", err)
			}
			if len(inner.sawEncode) == 0 {
				t.Error("the request never reached the codec — a gate that swallows a request " +
					"passes the weaker 'no error' check while serving nothing")
			}
		})
	}
}

// inlineDocs is a wire with no document part that carries textual documents as
// text instead of refusing them.
var inlineDocs = specutil.ContentPolicy{
	Allow:               map[string]bool{"text": true, "file": true},
	InlineTextDocuments: "this wire has no document part",
}

func fileBody(dataURL, filename string) string {
	name := ""
	if filename != "" {
		name = `,"filename":"` + filename + `"`
	}
	return `{"messages":[{"role":"user","content":[
		{"type":"file","file":{"file_data":"` + dataURL + `"` + name + `}}]}]}`
}

// TestInlineTextDocuments_JudgesTheDeclaredTypeAndTheBytes.
//
// The largest refusal class measured across the catalogue: a markdown
// attachment answers "Expected a base64-encoded data URL with an
// application/pdf MIME type" on a wire that would have read it fine as text.
// Carrying it as text is lossless — the bytes ARE characters.
//
// Both halves are asserted. The declared type has to be textual, because a PDF
// is not characters and sniffing cannot tell a small one from a text file. And
// the bytes have to BE text: declared textual with non-UTF-8 payload is left
// for the wire to refuse rather than inlined, since replacement characters in
// front of a model earn a confident answer about nothing.
func TestInlineTextDocuments_JudgesTheDeclaredTypeAndTheBytes(t *testing.T) {
	// "# Title" as text/markdown, with a filename the caller gave.
	got, res := encodeThrough(t, inlineDocs,
		fileBody("data:text/markdown;base64,IyBUaXRsZQ==", "notes.md"))

	if want := []string{"text"}; !slices.Equal(partTypes(got), want) {
		t.Fatalf("parts = %v, want %v — a markdown document reached a wire with no document "+
			"part and came back as that wire's own 400 about PDFs", partTypes(got), want)
	}
	text := gjson.GetBytes(got, "messages.0.content.0.text").String()
	if !strings.Contains(text, "# Title") {
		t.Errorf("the document's characters did not survive inlining: %q", text)
	}
	if !strings.Contains(text, "notes.md") {
		t.Errorf("the caller's filename is gone, so the model is shown a document with no "+
			"name and cannot refer to it: %q", text)
	}
	if len(res.Rewrites) == 0 {
		t.Error("the coercion is not announced, so a caller comparing what they sent with " +
			"what was served has nothing to read")
	}
}

// TestInlineTextDocuments_LeavesWhatItCannotConfirmIsText.
//
// Each of these must reach the wire unchanged rather than be inlined. A PDF is
// not characters. Bytes that are not valid UTF-8 would arrive as replacement
// characters. A reference with no bytes has nothing to inline.
func TestInlineTextDocuments_LeavesWhatItCannotConfirmIsText(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"a PDF is not characters", fileBody("data:application/pdf;base64,JVBERi0x", "r.pdf")},
		// 0xFF 0xFE 0xFD — valid base64, not valid UTF-8.
		{"declared text, bytes are not", fileBody("data:text/plain;base64,//79", "x.txt")},
		{"a file id carries no bytes", `{"messages":[{"role":"user","content":[
			{"type":"file","file":{"file_id":"file-abc"}}]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := encodeThrough(t, inlineDocs, tc.body)
			if want := []string{"file"}; !slices.Equal(partTypes(got), want) {
				t.Errorf("parts = %v, want %v — the gate inlined bytes it could not confirm "+
					"are text, putting them in front of the model as characters",
					partTypes(got), want)
			}
		})
	}
}

// TestOctetStream_IsRefusedWithSomethingTheCallerCanActOn.
//
// application/octet-stream says only that the type is unknown. Most HTTP
// clients attach it for any extension they do not recognise, so the caller
// usually does not know they omitted anything — and the wire's own answer names
// neither the file nor the omission.
func TestOctetStream_IsRefusedWithSomethingTheCallerCanActOn(t *testing.T) {
	codec, _ := gated(inlineDocs)
	// A payload that is not confirmable as text, so the octet-stream check is
	// what decides — declared-unknown bytes that happen to be text are carried.
	_, err := codec.EncodeRequest(typology.WireShapeOpenAIChat,
		[]byte(fileBody("data:application/octet-stream;base64,//79", "report")),
		provcore.CallTarget{})
	if err == nil {
		t.Fatal("an attachment of unknown type was forwarded; the wire answers about a type " +
			"the caller never chose to send")
	}
	if !strings.Contains(err.Error(), "application/octet-stream") {
		t.Errorf("the refusal does not name the declared type: %v", err)
	}
	if !strings.Contains(err.Error(), "text/markdown") {
		t.Errorf("the refusal does not say what to declare instead: %v", err)
	}
}

// TestOctetStream_TextBytesAreCarriedRatherThanRefused.
//
// Most HTTP clients attach application/octet-stream for any extension they do
// not recognise, so a caller sending a README usually did not choose that
// label. When the bytes ARE characters, refusing takes a document the wire
// would have read and turns it into our own 400 — measured on a document
// Gemini 3.x had been reading.
//
// The refusal is reserved for bytes we could not confirm are text, which the
// sibling test covers. Both paths are asserted because they are one line apart
// and look identical in a diff.
func TestOctetStream_TextBytesAreCarriedRatherThanRefused(t *testing.T) {
	// "hello" base64, declared as unknown bytes.
	got, res := encodeThrough(t, inlineDocs,
		fileBody("data:application/octet-stream;base64,aGVsbG8=", "README"))

	if want := []string{"text"}; !slices.Equal(partTypes(got), want) {
		t.Fatalf("parts = %v, want %v — a readable document mislabelled by the caller's HTTP "+
			"client became our own 400", partTypes(got), want)
	}
	text := gjson.GetBytes(got, "messages.0.content.0.text").String()
	if !strings.Contains(text, "hello") || !strings.Contains(text, "README") {
		t.Errorf("the document's characters or its name did not survive: %q", text)
	}
	if !slices.Contains(res.Rewrites, "octet_stream_inlined_as_text") {
		t.Errorf("rewrites = %v — the coercion is not announced", res.Rewrites)
	}
}

// IsTextualMediaType decides on the DECLARED type for both Cohere (no binary
// document at all) and Anthropic (its source shape depends on the answer), so
// the two cannot drift into one reading a .yaml the other refused. Structured
// suffixes count: application/vnd.api+json is text however it is spelled.
func TestIsTextualMediaType_CoversTheSpellingsCallersActuallySend(t *testing.T) {
	for _, mt := range []string{
		"text/plain", "TEXT/MARKDOWN", "  text/csv  ", "application/json",
		"application/yaml", "application/x-ndjson", "application/vnd.api+json",
		"application/problem+xml",
	} {
		if !specutil.IsTextualMediaType(mt) {
			t.Errorf("%q read as binary — the document is refused on one wire and inlined on "+
				"another, for the same bytes", mt)
		}
	}
	for _, mt := range []string{"application/pdf", "image/png", "application/octet-stream", ""} {
		if specutil.IsTextualMediaType(mt) {
			t.Errorf("%q read as text — its bytes go in front of the model as characters", mt)
		}
	}
}
