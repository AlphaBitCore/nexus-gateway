package codecs

import "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"

// registerMultimodalText wires the OpenAI multimodal text codecs — image
// generation (JSON both directions), TTS (JSON request / binary response), and
// STT transcription (response-only text). They are path-keyed like the
// embeddings codec so they override the adapter-only chat entry; critically the
// TTS response key stops the binary audio body from falling through to the chat
// codec, which would mislabel it `partial`. Each is registered for the
// openai-compatible family plus a path-only fallback for intercepted traffic
// whose adapter_type is a host label rather than a wire-format key.
func registerMultimodalText(reg *core.Registry, openAICompatible []string) {
	oaiImg := NewOpenAIImagesNormalizer()
	oaiTTS := NewOpenAIAudioSpeechNormalizer()
	oaiSTT := NewOpenAIAudioTranscriptionsNormalizer()
	for _, key := range openAICompatible {
		reg.Register(key+"::/v1/images/generations", oaiImg)
		reg.Register(key+"::/v1/audio/speech", oaiTTS)
		reg.Register(key+"::/v1/audio/transcriptions", oaiSTT)
		reg.Register(key+"::/v1/audio/translations", oaiSTT)
	}
	reg.Register("::/v1/images/generations", oaiImg)
	reg.Register("::/v1/audio/speech", oaiTTS)
	reg.Register("::/v1/audio/transcriptions", oaiSTT)
	reg.Register("::/v1/audio/translations", oaiSTT)
}
