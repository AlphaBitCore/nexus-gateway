package proxy

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// ExtractIngressModel returns the client-requested model and streaming
// flag for a request, given the ingress descriptor and the already-read
// body bytes. The model source is format-specific:
//
//   - openai, deepseek, anthropic, minimax, glm: body `model` field
//   - gemini: URL path value `{model}` — the captured segment is
//     `id:generateContent` or `id:streamGenerateContent`; the id is
//     derived by trimming that suffix.
//   - azure-openai: URL path value `{deployment}` (treated as model)
//
// The stream flag comes from the path (`:streamGenerateContent`) when
// [Ingress.StreamFromPath] is true; otherwise it is read from the body
// `stream` field for the formats that carry it there.
//
// An empty result is treated as a client error by the caller; this
// helper only distinguishes "no body field present" (empty string) from
// "path param missing" (returns an error).
func ExtractIngressModel(in Ingress, r *http.Request, body []byte) (modelID string, isStream bool, err error) {
	parsed := gjson.ParseBytes(body)
	isStream = in.StreamFromPath
	if !in.StreamFromPath && len(body) > 0 {
		isStream = parsed.Get("stream").Bool()
	}

	switch in.BodyFormat {
	case provcore.FormatOpenAI,
		provcore.FormatDeepSeek,
		provcore.FormatAnthropic,
		provcore.FormatMiniMax,
		provcore.FormatGLM,
		provcore.FormatOpenAIResponses:
		// /v1/responses uses the same top-level `model` field as
		// chat-completions; extraction is identical. The stream flag
		// reads from body `stream:true` (not from path).
		modelID = parsed.Get("model").String()
	case provcore.FormatGemini:
		seg := r.PathValue("model")
		if seg == "" {
			return "", false, fmt.Errorf("gemini ingress: {model} path parameter missing")
		}
		const genSuf = ":generateContent"
		const streamSuf = ":streamGenerateContent"
		switch {
		case strings.HasSuffix(seg, streamSuf):
			modelID = strings.TrimSuffix(seg, streamSuf)
		case strings.HasSuffix(seg, genSuf):
			modelID = strings.TrimSuffix(seg, genSuf)
		default:
			return "", false, fmt.Errorf("gemini ingress: path segment must end with %q or %q", genSuf, streamSuf)
		}
		if modelID == "" {
			return "", false, fmt.Errorf("gemini ingress: empty model id in path")
		}
	case provcore.FormatAzureOpenAI:
		modelID = r.PathValue("deployment")
		if modelID == "" {
			return "", false, fmt.Errorf("azure ingress: {deployment} path parameter missing")
		}
	case provcore.FormatBedrock, provcore.FormatVertex:
		return "", false, fmt.Errorf("%s ingress is not exposed in this release", in.BodyFormat)
	default:
		return "", false, fmt.Errorf("unsupported ingress format %q", in.BodyFormat)
	}

	return modelID, isStream, nil
}
