package deepseek

// codec_content.go — DeepSeek's chat body has no multimodal variants at all.
//
// Measured, and it is not a subtlety: the wire's deserializer rejects the part
// by NAME before it looks at anything inside it —
//
//	Failed to deserialize the JSON body into the target type:
//	messages[N]: unknown variant `image_url` … / unknown variant `file` …
//
// There is no documented mechanism for either, so this is the wire's limit
// rather than a form we are getting wrong. Forwarding it hands the caller a Rust
// enum error naming a variant list, which says nothing about their attachment.
//
// Enforced on both codec doors by the shared gate: the same-spec differential is
// the path an OpenAI-shaped request takes here, and it is the one that matters.

import "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"

func contentPolicy() specutil.ContentPolicy {
	return specutil.ContentPolicy{
		// Text only. Nothing is allowed beyond it.
		Allow: map[string]bool{},
		Deny: map[string]string{
			"image_url": "DeepSeek chat accepts text only — its request body has no image part. " +
				"Route an image to a vision model on another provider",
			"file": "DeepSeek chat accepts text only — its request body has no document part. " +
				"Send the document's text in the message, or route to a provider that reads " +
				"documents",
			"input_audio": "DeepSeek chat accepts text only — its request body has no audio part",
		},
	}
}
