package anthropic

// test_helpers_test.go wires private test-access aliases that let white-box
// tests (coverage_test.go, hub_ingress_test.go, stream_tooluse_test.go) call
// exported helpers from sub-packages using short unqualified names — matching
// the call-sites in those test files without requiring any import changes.

import (
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	apcodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/codec"
	specerrors "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/errors"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/ingress"
	apstream "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/stream"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/tidwall/gjson"
)

// codec is a package-level type alias that lets white-box tests in this
// package (coverage_test.go) reference codec{} without any import changes.
type codec = apcodec.Codec //nolint:golint,revive // intentional lower-case alias for test access

// errorNormalizer is a package-level type alias for coverage_test.go white-box tests.
type errorNormalizer = specerrors.ErrorNormalizer //nolint:golint,revive // intentional lower-case alias for test access

func stringifyOpenAIToolResultContent(c gjson.Result) string {
	return apcodec.StringifyOpenAIToolResultContent(c)
}

func parseDataURL(dataURL string) (mediaType, b64 string, ok bool) {
	return apcodec.ParseDataURL(dataURL)
}

func mapStopReason(r string) string {
	return apcodec.MapStopReason(r)
}

func usageToNormalize(u provcore.Usage) *normalize.Usage {
	return apcodec.UsageToNormalize(u)
}

func anthropicModelMaxOutput(model string) int {
	return apcodec.AnthropicModelMaxOutput(model)
}

func mapAnthropicStreamError(etype, emsg string) error {
	return apstream.MapAnthropicStreamError(etype, emsg)
}

func parseRetryAfter(v string) *time.Duration {
	return specerrors.ParseRetryAfter(v)
}

func stringifyAnthropicToolResult(c gjson.Result) string {
	return ingress.StringifyAnthropicToolResult(c)
}

func stringifyOpenAIMessageContent(content gjson.Result) string {
	return ingress.StringifyOpenAIMessageContent(content)
}

func mapOpenAIFinishToStopReason(r string) string {
	return ingress.MapOpenAIFinishToStopReason(r)
}
