package capability

// CarriedModalities decides what every capability check downstream is allowed
// to have an opinion about, and it had no direct test at all — it was exercised
// only through the resolver, whose fixtures happen to put one attachment in one
// user message. Two production mutations survived that arrangement completely:
// reading only the LAST message, and skipping any attachment whose custody is
// not "captured". Both are silent capability losses on real traffic.

import (
	"reflect"
	"testing"

	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func media(modality, source string) normcore.ContentBlock {
	return normcore.ContentBlock{MediaRef: &normcore.MediaRef{Modality: modality, Source: source}}
}

func msg(role normcore.Role, blocks ...normcore.ContentBlock) normcore.Message {
	return normcore.Message{Role: role, Content: blocks}
}

func payload(msgs ...normcore.Message) *normcore.NormalizedPayload {
	return &normcore.NormalizedPayload{Messages: msgs}
}

func TestCarriedModalities(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   *normcore.NormalizedPayload
		want []string
	}{
		{"nil payload", nil, nil},

		// Text is a modality the request carries, and reporting it is what lets
		// a floor predicate stay free of special cases. The proxy guard already
		// counted it; the router did not, and the two refused different
		// requests for the same model.
		{"a non-empty text block is reported", payload(msg("user",
			normcore.ContentBlock{Type: normcore.ContentText, Text: "hello"})), []string{"text"}},

		// An empty text block is not text. An image-only turn still carries a
		// text block on some ingress shapes, and counting it would satisfy a
		// text floor the request does not meet.
		{"an empty text block is not text", payload(msg("user",
			normcore.ContentBlock{Type: normcore.ContentText, Text: ""},
			media(normcore.ModalityImage, normcore.MediaCaptured))), []string{"image"}},

		{"one image", payload(msg("user",
			normcore.ContentBlock{Type: normcore.ContentText, Text: "what is this"},
			media(normcore.ModalityImage, normcore.MediaCaptured))), []string{"text", "image"}},

		// A conversation is not its last turn. An image attached three turns ago
		// is still in the context the model has to read, and a filter that stops
		// counting it routes the follow-up question to a model that cannot see
		// what is being asked about. Every multi-turn attachment request is
		// affected, and a single-message fixture cannot show it.
		{"an attachment in an EARLIER turn still counts", payload(
			msg("user", media(normcore.ModalityImage, normcore.MediaCaptured)),
			msg("assistant", normcore.ContentBlock{Text: "it is a cat"}),
			msg("user", normcore.ContentBlock{Text: "what colour?"}),
		), []string{"image"}},

		// Custody says where the BYTES are, not whether the request carries the
		// modality. A caller passing an image by URL, or one whose bytes aged out
		// of our own storage, is still asking a model to look at an image.
		// Filtering on Source silently un-sees every non-captured attachment.
		{"custody does not decide modality", payload(msg("user",
			media(normcore.ModalityImage, normcore.MediaExternal),
			media(normcore.ModalityAudio, normcore.MediaAgedOut),
		)), []string{"image", "audio"}},

		{"deduped, first-seen order", payload(
			msg("user", media(normcore.ModalityAudio, normcore.MediaCaptured)),
			msg("user", media(normcore.ModalityImage, normcore.MediaCaptured)),
			msg("user", media(normcore.ModalityAudio, normcore.MediaCaptured)),
		), []string{"audio", "image"}},

		// An empty Modality is not a modality. Counting "" would make every
		// candidate fail a dimension nothing can declare.
		{"a MediaRef with no modality is ignored", payload(msg("user",
			media("", normcore.MediaCaptured))), nil},

		// A media block carrying no MediaRef at all is the same silence as one
		// whose modality is blank, and a non-media block is not evidence of any
		// modality either. Reading a default out of an unlabelled attachment
		// would refuse candidates over content nobody described.
		{"unlabelled and non-media blocks contribute nothing", payload(msg("user",
			normcore.ContentBlock{Type: normcore.ContentMedia},
			normcore.ContentBlock{Type: normcore.ContentToolUse},
		)), nil},

		{"documents come through as file", payload(msg("user",
			media(normcore.ModalityFile, normcore.MediaCaptured))), []string{"file"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CarriedModalities(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("CarriedModalities() = %v, want %v", got, tc.want)
			}
		})
	}
}
