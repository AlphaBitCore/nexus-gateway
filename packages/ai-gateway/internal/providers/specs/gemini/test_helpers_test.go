package gemini

// test_helpers_test.go wires private test-access aliases that let white-box
// tests (coverage_test.go, codec_test.go, hub_ingress_test.go, stream_test.go)
// call exported helpers from sub-packages using short unqualified names —
// matching the call-sites in those test files without requiring import changes.

import (
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	gcodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/codec"
	specerrors "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/errors"
	gstream "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/stream"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// codec is a package-level type alias that lets white-box tests in this
// package (coverage_test.go, codec_test.go) reference codec{} without any
// import changes. It aliases gcodec.Codec from the internal sub-package.
type codec = gcodec.Codec //nolint:golint,revive // intentional lower-case alias for test access

// errorNormalizer is a package-level type alias for coverage_test.go white-box tests.
type errorNormalizer = specerrors.ErrorNormalizer //nolint:golint,revive // intentional lower-case alias for test access

func parseRetryAfter(v string) *time.Duration {
	return specerrors.ParseRetryAfter(v)
}

func parseDataURL(dataURL string) (mediaType, b64 string, ok bool) {
	return gcodec.ParseDataURL(dataURL)
}

func guessMimeFromURL(u string) string {
	return gcodec.GuessMimeFromURL(u)
}

func mapFinishReason(r string) string {
	return gcodec.MapFinishReason(r)
}

func usageToNormalize(u provcore.Usage) *normalize.Usage {
	return gcodec.UsageToNormalize(u)
}

func formatSSE(event string, data []byte) []byte {
	return gstream.FormatSSE(event, data)
}
