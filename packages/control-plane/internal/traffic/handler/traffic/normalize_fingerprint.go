package traffic

import (
	"encoding/json"
	"strings"

	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// artifactRef mirrors the shape the gateway writes into
// traffic_event.artifact_refs for a binary it hashed but did not keep.
type artifactRef struct {
	Sha256    string `json:"sha256,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	Mime      string `json:"mime,omitempty"`
	URL       string `json:"url,omitempty"`
}

// fingerprintRequestPayload turns request-side artifact fingerprints into a
// normalized payload carrying one media element each.
//
// It exists so that media lives in exactly one place. An STT request stores
// no body — the audio is hashed, forwarded, and released, deliberately,
// because the audit pool is byte-bounded and these uploads reach tens of
// megabytes. Before this, that produced an event with no request payload at
// all: the fingerprint went to the database and was read by nothing, so a
// transcription looked like a request with no audio in it.
//
// The alternative was a second UI surface reading artifact_refs directly.
// That would have put the same artifact on screen twice for the endpoints
// whose refs describe a response, with two different custody states — which
// is the shape this whole area is being unpicked to remove.
func fingerprintRequestPayload(raw string) json.RawMessage {
	var refs []artifactRef
	if err := json.Unmarshal([]byte(raw), &refs); err != nil {
		return nil
	}

	var blocks []normcore.ContentBlock
	for _, r := range refs {
		// An entry with neither a digest nor a URL describes nothing.
		if r.Sha256 == "" && r.URL == "" {
			continue
		}
		ref := &normcore.MediaRef{
			Modality:  modalityForMime(r.Mime),
			Mime:      r.Mime,
			SizeBytes: r.SizeBytes,
			SHA256:    r.Sha256,
			// Two different facts, kept apart: a URL-bearing entry names
			// something the gateway never fetched, while a digest-only entry
			// names something it read and did not retain. Neither can be
			// opened, and neither carries a locator, so no control is
			// offered for either.
			Source: normcore.MediaFingerprint,
			URL:    r.URL,
			Cause:  "not-retained",
		}
		if r.URL != "" {
			ref.Source = normcore.MediaExternal
			ref.Cause = ""
		}
		blocks = append(blocks, normcore.ContentBlock{Type: normcore.ContentMedia, MediaRef: ref})
	}
	if len(blocks) == 0 {
		return nil
	}

	payload := normcore.NormalizedPayload{
		Kind:             normcore.KindAIChat,
		NormalizeVersion: normcore.SchemaVersion,
		Messages: []normcore.Message{{
			Role:    normcore.RoleUser,
			Content: blocks,
		}},
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return out
}

// modalityForMime is the same mapping the codecs use, so a card built from a
// fingerprint reads identically to one built from captured bytes. An unknown
// type is a file, never a guess.
func modalityForMime(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return normcore.ModalityImage
	case strings.HasPrefix(mime, "audio/"):
		return normcore.ModalityAudio
	case strings.HasPrefix(mime, "video/"):
		return normcore.ModalityVideo
	}
	return normcore.ModalityFile
}
