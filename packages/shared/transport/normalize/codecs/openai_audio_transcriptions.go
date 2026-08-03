package codecs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// OpenAIAudioTranscriptionsNormalizer handles OpenAI's /v1/audio/transcriptions
// and /v1/audio/translations (STT) surface.
//
// It is RESPONSE-ONLY for structured text. The request is a multipart audio
// upload — the boundary is stripped before any codec runs and the audio bytes
// are deliberately not captured (fingerprint only), so there is no request-side
// text to project. The request direction returns a terminal multipart summary
// rather than declining, so a request body that IS present on the interception
// path never falls through to the adapter-only chat codec (walk-trap guard).
//
// Response shapes:
//   - json:          {"text":"the transcript"}
//   - verbose_json:  {"text":"...","segments":[{"text":".."},...],"duration":..}
//   - text:          the raw transcript (text/plain body)
//
// The transcript is projected as assistant text. Kind = ai-stt.
type OpenAIAudioTranscriptionsNormalizer struct{}

// NewOpenAIAudioTranscriptionsNormalizer returns a stateless normalizer.
func NewOpenAIAudioTranscriptionsNormalizer() *OpenAIAudioTranscriptionsNormalizer {
	return &OpenAIAudioTranscriptionsNormalizer{}
}

// ID is the metric / log label.
func (n *OpenAIAudioTranscriptionsNormalizer) ID() string { return "openai-audio-transcriptions" }

// Normalize routes by Meta.Direction. Neither direction returns ErrUnsupported
// on a non-empty body (walk-trap guard).
func (n *OpenAIAudioTranscriptionsNormalizer) Normalize(_ context.Context, raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	if len(raw) == 0 {
		return zeroSTTPayload(meta), fmt.Errorf("openai-audio-transcriptions: empty body: %w", core.ErrUnsupported)
	}
	if meta.Direction == core.DirectionRequest {
		// Multipart audio upload — summarize terminally, never project or decline.
		mime := meta.ContentType
		if mime == "" {
			mime = "multipart/form-data"
		}
		sum := sha256.Sum256(raw)
		return core.NormalizedPayload{
			Kind:             core.KindHTTPMultipart,
			NormalizeVersion: core.SchemaVersion,
			Protocol:         "openai-audio-transcriptions",
			DetectedSpec:     "openai-audio-transcriptions",
			HTTP: &core.HTTPPayload{BodyView: &core.HTTPBodyView{BinaryRef: &core.BinaryRef{
				Size:        int64(len(raw)),
				ContentType: mime,
				SHA256:      hex.EncodeToString(sum[:]),
			}}},
		}, nil
	}

	transcript := decodeTranscript(raw, meta.ContentType)
	out := core.NormalizedPayload{
		Kind:             core.KindAISTT,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "openai-audio-transcriptions",
		DetectedSpec:     "openai-audio-transcriptions",
		Model:            meta.Model,
	}
	if transcript != "" {
		out.Messages = []core.Message{{Role: core.RoleAssistant, Content: textBlocks([]string{transcript})}}
		// A transcript was extracted (top-level `text`, joined `segments`, or a
		// text/plain body): this codec is the authoritative decoder for the
		// keyed path, so claim it. Scoring by the `text` field spec would sink a
		// segments-only `verbose_json` body to 0.55 and soft-fall it through to
		// generic-http, losing the ai-stt render.
		out.Confidence = 1.0
	} else {
		out.Confidence = core.ScoreTier1Confidence(raw, core.FieldSpec{
			Required: []string{"text"},
			Optional: []string{"segments", "duration", "language", "words", "task"},
		})
	}
	return out, nil
}

// sttResponse mirrors the json / verbose_json transcription response.
type sttResponse struct {
	Text     string       `json:"text"`
	Segments []sttSegment `json:"segments,omitempty"`
}

type sttSegment struct {
	Text string `json:"text"`
}

// decodeTranscript extracts the transcript from a json / verbose_json body, or
// treats the raw bytes as the transcript for a text/plain response. When JSON
// parses and carries `text`, that wins; otherwise the segments are joined; a
// non-JSON body (response_format=text) is returned verbatim (trimmed).
func decodeTranscript(raw []byte, contentType string) string {
	if strings.HasPrefix(contentType, "text/") {
		return strings.TrimSpace(string(raw))
	}
	var r sttResponse
	if err := json.Unmarshal(raw, &r); err == nil {
		if r.Text != "" {
			return r.Text
		}
		if len(r.Segments) > 0 {
			parts := make([]string, 0, len(r.Segments))
			for _, s := range r.Segments {
				if s.Text != "" {
					parts = append(parts, s.Text)
				}
			}
			if joined := strings.TrimSpace(strings.Join(parts, "")); joined != "" {
				return joined
			}
		}
		// JSON parsed but carried no recognized transcript field: a
		// non-conforming provider shape ({"result":"…"} etc.). Fall through to
		// the raw text so the content stays scannable on the interception path
		// — over-scanning is safer than a silent DLP gap.
	}
	// Not JSON and not declared text/*: response_format=text sends a bare
	// transcript with an unhelpful content-type; treat it as the transcript.
	return strings.TrimSpace(string(raw))
}

func zeroSTTPayload(meta core.Meta) core.NormalizedPayload {
	return core.NormalizedPayload{
		Kind:             core.KindAISTT,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "openai-audio-transcriptions",
		Model:            meta.Model,
	}
}
