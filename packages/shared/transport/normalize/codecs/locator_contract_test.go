package codecs

import (
	"context"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/locator"
)

// The contract this file exists for:
//
//	offersBytes(ref)  ⟺  the locator resolves from the same stored bytes,
//	                     and yields exactly SizeBytes of them.
//
// where offersBytes is `source == captured && !truncated` — the same
// predicate mediaHasBytes uses to decide whether a card gets a Preview or a
// Download at all. Captured-and-truncated is a real and deliberate third
// state: the bytes ARE at that locator and are not fully recoverable, which
// the card says out loud with its cause instead of pretending they were
// never there.
//
// Everything else in this area is a detail of how a particular provider
// spells a particular field. This is the property a user actually feels: a
// card that offers Preview or Download must never fail when it is clicked,
// and a card that offers nothing must be telling the truth about why.
//
// Before the locator package there was no way to state it. The codecs
// promised bytes were recoverable and a separate endpoint decided whether
// they were, so the two could — and repeatedly did — disagree. Now the
// promise and the resolution are the same code, and this asserts that they
// stay pointed at the same bytes.

type locatorCase struct {
	name string
	body string
	norm func([]byte) (core.NormalizedPayload, error)
}

func req(n interface {
	Normalize(context.Context, []byte, core.Meta) (core.NormalizedPayload, error)
}) func([]byte) (core.NormalizedPayload, error) {
	return func(b []byte) (core.NormalizedPayload, error) {
		return n.Normalize(context.Background(), b, core.Meta{Direction: core.DirectionRequest})
	}
}

func resp(n interface {
	Normalize(context.Context, []byte, core.Meta) (core.NormalizedPayload, error)
}) func([]byte) (core.NormalizedPayload, error) {
	return func(b []byte) (core.NormalizedPayload, error) {
		return n.Normalize(context.Background(), b, core.Meta{Direction: core.DirectionResponse})
	}
}

func streamResp(n interface {
	Normalize(context.Context, []byte, core.Meta) (core.NormalizedPayload, error)
}) func([]byte) (core.NormalizedPayload, error) {
	return func(b []byte) (core.NormalizedPayload, error) {
		return n.Normalize(context.Background(), b, core.Meta{Direction: core.DirectionResponse, Stream: true})
	}
}

// A 1x1 PNG, real bytes, so a resolved artifact is something a viewer could
// actually open rather than a base64 shape that happens to decode.
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func locatorCases() []locatorCase {
	return []locatorCase{
		{
			"openai chat image_url",
			`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + onePixelPNG + `"}}]}]}`,
			req(NewOpenAIChatNormalizer()),
		},
		{
			"openai chat input_audio",
			`{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"` + onePixelPNG + `","format":"wav"}}]}]}`,
			req(NewOpenAIChatNormalizer()),
		},
		{
			"openai chat file_data",
			`{"model":"m","messages":[{"role":"user","content":[{"type":"file","file":{"file_data":"data:application/pdf;base64,` + onePixelPNG + `","filename":"a.pdf"}}]}]}`,
			req(NewOpenAIChatNormalizer()),
		},
		{
			"openai responses input_image",
			`{"model":"m","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,` + onePixelPNG + `"}]}]}`,
			req(NewOpenAIResponsesNormalizer()),
		},
		{
			"openai responses input_file",
			`{"model":"m","input":[{"role":"user","content":[{"type":"input_file","file_data":"data:application/pdf;base64,` + onePixelPNG + `"}]}]}`,
			req(NewOpenAIResponsesNormalizer()),
		},
		{
			"anthropic image source",
			`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + onePixelPNG + `"}}]}]}`,
			req(NewAnthropicMessagesNormalizer()),
		},
		{
			"anthropic document source",
			`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + onePixelPNG + `"}}]}]}`,
			req(NewAnthropicMessagesNormalizer()),
		},
		{
			"cohere image_url",
			`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + onePixelPNG + `"}}]}]}`,
			req(NewCohereChatNormalizer()),
		},
		{
			"gemini inlineData",
			`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"` + onePixelPNG + `"}}]}]}`,
			req(NewGeminiGenerateNormalizer()),
		},
		{
			"gemini streamed inlineData",
			"data: " + `{"candidates":[{"content":{"role":"model","parts":[{"text":"here"}]}}]}` + "\n\n" +
				"data: " + `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"` + onePixelPNG + `"}}]},"finishReason":"STOP"}]}` + "\n\n",
			streamResp(NewGeminiGenerateNormalizer()),
		},
		{
			"replicate inline output",
			`{"status":"succeeded","output":"data:image/png;base64,` + onePixelPNG + `"}`,
			resp(NewReplicateNormalizer()),
		},
		{
			"replicate output array",
			`{"status":"succeeded","output":["data:image/png;base64,` + onePixelPNG + `","data:image/png;base64,` + onePixelPNG + `"]}`,
			resp(NewReplicateNormalizer()),
		},
		{
			"openai chat audio response",
			`{"choices":[{"message":{"role":"assistant","audio":{"id":"a","data":"` + onePixelPNG + `","transcript":"hi","expires_at":1}}}]}`,
			resp(NewOpenAIChatNormalizer()),
		},
	}
}

// Captured means recoverable. Not "we saw bytes", not "there was a field" —
// recoverable, from the stored body, right now, at exactly the size the ref
// reports.
func TestEveryCapturedRefResolvesToItsOwnBytes(t *testing.T) {
	for _, tc := range locatorCases() {
		t.Run(tc.name, func(t *testing.T) {
			p, err := tc.norm([]byte(tc.body))
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			refs := allRefs(p)
			if len(refs) == 0 {
				t.Fatal("fixture produced no media ref, so it asserts nothing")
			}
			var captured int
			for i, ref := range refs {
				if !offersBytes(ref) {
					continue
				}
				captured++
				if ref.Locator == "" {
					t.Fatalf("ref %d claims captured bytes with no way to reach them: %+v", i, ref)
				}
				got, err := locator.Resolve([]byte(tc.body), ref.Locator)
				if err != nil {
					t.Fatalf("ref %d promised %q and it does not resolve: %v", i, ref.Locator, err)
				}
				if int64(len(got.Bytes)) != ref.SizeBytes {
					t.Fatalf("ref %d says %d bytes, the locator yields %d — the size a user sees is not the size they would get",
						i, ref.SizeBytes, len(got.Bytes))
				}
				if ref.Cause != "" {
					t.Fatalf("ref %d offers bytes yet names a failure: %+v", i, ref)
				}
			}
			if captured == 0 {
				t.Fatal("no ref in this fixture offers bytes, so the contract is not exercised")
			}
		})
	}
}

// The other half of the same contract. A ref that cannot offer bytes must
// say so, rather than carrying a locator that would fail when followed.
func TestNonCapturedRefsDoNotCarryABrokenPromise(t *testing.T) {
	for _, tc := range []locatorCase{
		{
			"external url",
			`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://r.example/a.png"}}]}]}`,
			req(NewOpenAIChatNormalizer()),
		},
		{
			"provider-held file",
			`{"contents":[{"role":"user","parts":[{"fileData":{"mimeType":"video/mp4","fileUri":"https://generativelanguage.googleapis.com/v1beta/files/x"}}]}]}`,
			req(NewGeminiGenerateNormalizer()),
		},
		{
			"undecodable payload",
			`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,!!!!"}}]}]}`,
			req(NewOpenAIChatNormalizer()),
		},
		{
			"empty payload",
			`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,"}}]}]}`,
			req(NewOpenAIChatNormalizer()),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := tc.norm([]byte(tc.body))
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			refs := allRefs(p)
			if len(refs) == 0 {
				t.Fatal("fixture produced no media ref, so it asserts nothing")
			}
			for i, ref := range refs {
				if offersBytes(ref) {
					t.Fatalf("ref %d should not offer bytes: %+v", i, ref)
				}
				// Whatever it does say, it must name WHY, or the reader is
				// left with an artifact that silently does nothing. External
				// and provider-held refs are exempt: their source IS the
				// explanation, and both carry the reference itself.
				switch ref.Source {
				case core.MediaExternal:
					if ref.URL == "" {
						t.Fatalf("ref %d is external with nothing to point at: %+v", i, ref)
					}
				case core.MediaProviderRef:
					if ref.ProviderRef == "" {
						t.Fatalf("ref %d is provider-held with no reference: %+v", i, ref)
					}
				default:
					if ref.Cause == "" {
						t.Fatalf("ref %d withholds bytes without saying why: %+v", i, ref)
					}
				}
				if ref.Locator == "" {
					continue
				}
				// A locator that survives on a ref offering nothing is
				// allowed — it records WHERE we looked — but it must not
				// resolve, or the card and the endpoint disagree again.
				if _, err := locator.Resolve([]byte(tc.body), ref.Locator); err == nil {
					t.Fatalf("ref %d offers nothing yet its locator serves bytes: %+v", i, ref)
				}
			}
		})
	}
}

// Whatever the containers grow to, a locator is resolvable from the stored
// bytes alone. That is the property that lets the same string mean the same
// artifact in the Control Plane, in the agent, and in a browser — and the
// reason the agent needed no new API to render media.
func TestEveryEmittedLocatorUsesAKnownContainer(t *testing.T) {
	known := []string{locator.Body, "json:", "datauri:", "sse:", "multipart:"}
	for _, tc := range locatorCases() {
		p, err := tc.norm([]byte(tc.body))
		if err != nil {
			t.Fatalf("%s: normalize: %v", tc.name, err)
		}
		for _, ref := range allRefs(p) {
			if ref.Locator == "" {
				continue
			}
			var ok bool
			for _, k := range known {
				if ref.Locator == k || strings.HasPrefix(ref.Locator, k) {
					ok = true
					break
				}
			}
			if !ok {
				t.Fatalf("%s emitted %q, which no resolver can read", tc.name, ref.Locator)
			}
		}
	}
}

// offersBytes mirrors the UI's mediaHasBytes. Keeping the Go contract keyed
// on the same predicate the card renders from is the point: a test that
// asserted "captured" alone would pass while the button stayed grey.
func offersBytes(ref *core.MediaRef) bool {
	return ref.Source == core.MediaCaptured && !ref.Truncated
}

func allRefs(p core.NormalizedPayload) []*core.MediaRef {
	var out []*core.MediaRef
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if b.MediaRef != nil {
				out = append(out, b.MediaRef)
			}
		}
	}
	return out
}

// The STT case, end to end. The gateway now hands the audio to the audit
// record instead of only fingerprinting it, and the generic binary path
// already turns a body with a real content type into an addressable ref —
// so the modality that could previously only be PROVEN untampered can now
// also be opened, with no codec written for it.
func TestWholeBodyAudioIsCapturedAndAddressable(t *testing.T) {
	// A minimal MP3 frame header: enough that a served artifact sniffs as
	// audio rather than as an opaque blob.
	audio := append([]byte{0xFF, 0xFB, 0x90, 0x00}, make([]byte, 60)...)

	p, err := NewGenericHTTPNormalizer().Normalize(context.Background(), audio,
		core.Meta{Direction: core.DirectionRequest, ContentType: "audio/mpeg"})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if p.HTTP == nil || p.HTTP.BodyView == nil || p.HTTP.BodyView.MediaRef == nil {
		t.Fatalf("an audio body must project a media ref: %+v", p)
	}
	ref := p.HTTP.BodyView.MediaRef
	if ref.Modality != core.ModalityAudio || ref.Source != core.MediaCaptured {
		t.Fatalf("ref = %+v", ref)
	}
	if ref.SHA256 == "" {
		t.Fatalf("the digest is what proves the file was not altered: %+v", ref)
	}
	// The contract every other captured ref is held to: the locator resolves,
	// to exactly the bytes the ref describes.
	got, err := locator.Resolve(audio, ref.Locator)
	if err != nil {
		t.Fatalf("ref promised %q and it does not resolve: %v", ref.Locator, err)
	}
	if int64(len(got.Bytes)) != ref.SizeBytes || string(got.Bytes) != string(audio) {
		t.Fatalf("resolved %d bytes, ref says %d", len(got.Bytes), ref.SizeBytes)
	}
}
