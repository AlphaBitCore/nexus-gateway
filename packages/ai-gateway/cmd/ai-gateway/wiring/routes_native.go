// routes_native.go — vendor-native wire-shape ingress routes (Gemini, Azure
// OpenAI, GLM), split out of routes.go to keep MountCoreRoutes under the
// file-size ratchet cap.
package wiring

import (
	"net/http"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/envelope"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/proxy"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// mountNativeProviderRoutes registers the vendor-native wire-shape ingress
// routes (Gemini, Azure OpenAI, GLM) that accept a provider's own request
// shape directly, rather than the OpenAI-canonical /v1/* routes.
func mountNativeProviderRoutes(mux *http.ServeMux, proxyHandler *proxy.Handler) {
	// Gemini native ingress.
	mux.HandleFunc("POST /v1beta/models/{model}", func(w http.ResponseWriter, r *http.Request) {
		full := r.PathValue("model")
		switch {
		case strings.HasSuffix(full, ":streamGenerateContent"):
			proxyHandler.ServeProxy(proxy.Ingress{
				WireShape:      typology.WireShapeGeminiGenerateContent,
				BodyFormat:     provcore.FormatGemini,
				Stream:         true,
				StreamFromPath: true,
			})(w, r)
		case strings.HasSuffix(full, ":generateContent"):
			proxyHandler.ServeProxy(proxy.Ingress{
				WireShape:  typology.WireShapeGeminiGenerateContent,
				BodyFormat: provcore.FormatGemini,
			})(w, r)
		default:
			// Not http.NotFound: that answers text/plain, and a Gemini client
			// JSON-parses the body, so a mistyped action reached it as a 404
			// carrying no message.
			envelope.WriteEndpointNotSupported(w, r.URL.Path)
		}
	})

	// Azure OpenAI native ingress.
	mux.HandleFunc("POST /openai/deployments/{deployment}/chat/completions", proxyHandler.ServeProxy(proxy.Ingress{
		WireShape: typology.WireShapeOpenAIChat, BodyFormat: provcore.FormatAzureOpenAI,
	}))
	mux.HandleFunc("POST /openai/deployments/{deployment}/embeddings", proxyHandler.ServeProxy(proxy.Ingress{
		WireShape: typology.WireShapeOpenAIEmbeddings, BodyFormat: provcore.FormatAzureOpenAI,
	}))

	// GLM (ZhipuAI) native ingress.
	mux.HandleFunc("POST /api/paas/v4/chat/completions", proxyHandler.ServeProxy(proxy.Ingress{
		WireShape: typology.WireShapeOpenAIChat, BodyFormat: provcore.FormatGLM,
	}))
	mux.HandleFunc("POST /api/paas/v4/embeddings", proxyHandler.ServeProxy(proxy.Ingress{
		WireShape: typology.WireShapeOpenAIEmbeddings, BodyFormat: provcore.FormatGLM,
	}))
}
