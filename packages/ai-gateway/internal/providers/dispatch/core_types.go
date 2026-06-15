// Package-internal type aliases that re-export the core package's public
// surface into the dispatch package namespace. This lets spec_adapter.go,
// the test helpers, and the metrics shim use unqualified names (Adapter,
// AdapterSpec, Format, …) exactly as they did before the core/dispatch split,
// without importing core at every call site.
//
// Note: Go type aliases and constant aliases are used here; these are
// zero-cost compile-time conveniences and do not introduce a runtime
// indirection.
package dispatch

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// Type aliases for the core package's public types.

type Adapter = core.Adapter
type AdapterSpec = core.AdapterSpec
type Format = core.Format
type CallTarget = core.CallTarget
type Request = core.Request
type Response = core.Response
type Usage = core.Usage
type EncodeResult = core.EncodeResult
type DecodeResult = core.DecodeResult
type DecodeContext = core.DecodeContext
type StreamSession = core.StreamSession
type Chunk = core.Chunk
type ToolCallDelta = core.ToolCallDelta
type ProbeResult = core.ProbeResult
type ProviderError = core.ProviderError
type Transport = core.Transport
type SchemaCodec = core.SchemaCodec
type StreamDecoder = core.StreamDecoder
type ErrorNormalizer = core.ErrorNormalizer

// Format constants forwarded from core.

const (
	FormatOpenAI          = core.FormatOpenAI
	FormatDeepSeek        = core.FormatDeepSeek
	FormatGLM             = core.FormatGLM
	FormatAzureOpenAI     = core.FormatAzureOpenAI
	FormatAnthropic       = core.FormatAnthropic
	FormatGemini          = core.FormatGemini
	FormatMiniMax         = core.FormatMiniMax
	FormatBedrock         = core.FormatBedrock
	FormatVertex          = core.FormatVertex
	FormatCohere          = core.FormatCohere
	FormatHuggingFace     = core.FormatHuggingFace
	FormatReplicate       = core.FormatReplicate
	FormatMistral         = core.FormatMistral
	FormatXai             = core.FormatXai
	FormatGroq            = core.FormatGroq
	FormatPerplexity      = core.FormatPerplexity
	FormatTogether        = core.FormatTogether
	FormatFireworks       = core.FormatFireworks
	FormatMoonshot        = core.FormatMoonshot
	FormatOpenAIResponses = core.FormatOpenAIResponses
)

// Error code constants forwarded from core.

const (
	CodeInvalidRequest       = core.CodeInvalidRequest
	CodeAuthFailed           = core.CodeAuthFailed
	CodeRateLimited          = core.CodeRateLimited
	CodeTimeout              = core.CodeTimeout
	CodeUpstreamError        = core.CodeUpstreamError
	CodeEndpointUnsupported  = core.CodeEndpointUnsupported
	CodeNotImplemented       = core.CodeNotImplemented
	CodeNoCompatibleProvider = core.CodeNoCompatibleProvider
)

// Other core helpers forwarded into the dispatch namespace.

var AllFormats = core.AllFormats
var LimitedReadAll = core.LimitedReadAll
var LimitedReadAllN = core.LimitedReadAllN
var ExtractUsage = core.ExtractUsage
var NewRegistry = core.NewRegistry

// Registry is a type alias for core.Registry.
type Registry = core.Registry

const ReadAllLimit = core.ReadAllLimit
