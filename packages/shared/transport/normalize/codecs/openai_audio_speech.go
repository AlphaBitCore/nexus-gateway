package codecs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// OpenAIAudioSpeechNormalizer handles OpenAI's /v1/audio/speech (TTS) surface.
//
// Request (application/json): {"model":"tts-1","input":"...","instructions":"..","voice":"alloy",...}
// The user-text fields are `input` (the synthesized text) AND `instructions`
// (voice-steering free text on gpt-4o-mini-tts). Kind = ai-tts.
//
// Response: binary audio (audio/mpeg, audio/wav, …). This normalizer CLAIMS the
// response terminally as an http-binary summary (size + content-type) rather
// than returning ErrUnsupported — a decline would fall through to the
// adapter-only OpenAI chat codec, whose hard unmarshal error on the binary
// bytes stops the tier walk and mislabels the row `partial`. Terminal claim is
// the fix for that misdetect.
type OpenAIAudioSpeechNormalizer struct{}

// NewOpenAIAudioSpeechNormalizer returns a stateless normalizer instance.
func NewOpenAIAudioSpeechNormalizer() *OpenAIAudioSpeechNormalizer {
	return &OpenAIAudioSpeechNormalizer{}
}

// ID is the metric / log label.
func (n *OpenAIAudioSpeechNormalizer) ID() string { return "openai-audio-speech" }

// Normalize routes by Meta.Direction. The response direction never returns
// ErrUnsupported (see the type doc).
func (n *OpenAIAudioSpeechNormalizer) Normalize(_ context.Context, raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	if len(raw) == 0 {
		return zeroTTSPayload(meta), fmt.Errorf("openai-audio-speech: empty body: %w", core.ErrUnsupported)
	}
	switch meta.Direction {
	case core.DirectionRequest:
		p, err := n.normalizeRequest(raw, meta)
		if err == nil {
			p.Confidence = core.ScoreTier1Confidence(raw, core.FieldSpec{
				Required: []string{"input"},
				Optional: []string{"model", "voice", "instructions", "response_format", "speed"},
			})
			p.DetectedSpec = "openai-audio-speech"
		}
		return p, err
	case core.DirectionResponse:
		// Terminal binary summary — MUST NOT decline (walk-trap guard).
		mime := meta.ContentType
		if mime == "" {
			mime = "audio/mpeg"
		}
		sum := sha256.Sum256(raw)
		return core.NormalizedPayload{
			Kind:             core.KindHTTPBinary,
			NormalizeVersion: core.SchemaVersion,
			Protocol:         "openai-audio-speech",
			DetectedSpec:     "openai-audio-speech",
			HTTP: &core.HTTPPayload{BodyView: &core.HTTPBodyView{BinaryRef: &core.BinaryRef{
				Size:        int64(len(raw)),
				ContentType: mime,
				SHA256:      hex.EncodeToString(sum[:]),
			}}},
		}, nil
	default:
		return zeroTTSPayload(meta), fmt.Errorf("openai-audio-speech: direction %q not supported: %w", meta.Direction, core.ErrUnsupported)
	}
}

// openAIAudioSpeechRequest mirrors the request body.
type openAIAudioSpeechRequest struct {
	Model        string `json:"model"`
	Input        string `json:"input"`
	Instructions string `json:"instructions,omitempty"`
}

func (n *OpenAIAudioSpeechNormalizer) normalizeRequest(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var req openAIAudioSpeechRequest
	if err := decodeLenient(raw, &req); err != nil {
		return zeroTTSPayload(meta), fmt.Errorf("openai-audio-speech: request unmarshal: %w", err)
	}
	if req.Input == "" && req.Instructions == "" {
		return zeroTTSPayload(meta), fmt.Errorf("openai-audio-speech: no user text: %w", core.ErrUnsupported)
	}
	var texts []string
	if req.Input != "" {
		texts = append(texts, req.Input)
	}
	if req.Instructions != "" {
		texts = append(texts, req.Instructions)
	}
	return core.NormalizedPayload{
		Kind:             core.KindAITTS,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "openai-audio-speech",
		Model:            firstNonEmpty(req.Model, meta.Model),
		Messages:         []core.Message{{Role: core.RoleUser, Content: textBlocks(texts)}},
	}, nil
}

func zeroTTSPayload(meta core.Meta) core.NormalizedPayload {
	return core.NormalizedPayload{
		Kind:             core.KindAITTS,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "openai-audio-speech",
		Model:            meta.Model,
	}
}
