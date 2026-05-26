package wiring

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/intercept"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
)

// InspectRequest runs the compliance hook pipeline on a decrypted HTTP request
// body after TLS MITM. Implements platform.RequestInspector.
func (b *ConnectionBridge) InspectRequest(ctx context.Context, host, method, path string, headers http.Header, body []byte) api.InspectionResult {
	result := b.InterceptHandler.ProcessRequest(ctx, host, method, path, headers, body)

	decision := "approve"
	switch string(result.Decision) {
	case "REJECT_HARD":
		decision = "reject_hard"
	case "BLOCK_SOFT":
		decision = "block_soft"
	}

	slog.Debug("request inspection",
		"host", host,
		"method", method,
		"path", path,
		"decision", decision,
		"provider", result.Provider,
		"model", result.Model,
		"bodySize", len(body),
	)
	return api.InspectionResult{
		Decision:          decision,
		Reason:            result.Reason,
		ComplianceTags:    result.ComplianceTags,
		HookOutcome:       result.HookOutcome,
		Provider:          result.Provider,
		Model:             result.Model,
		ApiKeyClass:       result.ApiKeyClass,
		ApiKeyFingerprint: result.ApiKeyFingerprint,
		PayloadRequest:    intercept.CaptureRequestBody(b.PayloadCaptureStore, body),
	}
}

// InspectResponse runs the compliance hook pipeline on a decrypted HTTP
// response body after TLS MITM. Implements platform.ResponseInspector.
func (b *ConnectionBridge) InspectResponse(ctx context.Context, host, method, path string, body []byte, usage *traffic.UsageMeta) api.InspectionResult {
	if b.InterceptHandler == nil {
		return api.InspectionResult{Decision: "approve"}
	}

	if usage == nil && len(body) > 0 {
		um := b.InterceptHandler.ExtractResponseUsage(ctx, host, path, nil, body)
		usage = &um
	}

	result := b.InterceptHandler.ProcessResponse(ctx, host, path, body, usage)

	decision := "approve"
	switch string(result.Decision) {
	case "REJECT_HARD":
		decision = "reject_hard"
	case "BLOCK_SOFT":
		decision = "block_soft"
	}

	return api.InspectionResult{
		Decision:              decision,
		Reason:                result.Reason,
		ComplianceTags:        result.ComplianceTags,
		HookOutcome:           result.HookOutcome,
		PromptTokens:          result.PromptTokens,
		CompletionTokens:      result.CompletionTokens,
		UsageExtractionStatus: result.UsageExtractionStatus,
		PayloadResponse:       intercept.CaptureResponseBody(b.PayloadCaptureStore, body),
	}
}

// NewUsageAccumulator returns a streaming UsageAccumulator for the given
// provider/model pair. Implements platform.ResponseUsageDetector.
func (b *ConnectionBridge) NewUsageAccumulator(provider, model string) streaming.UsageAccumulator {
	if b.InterceptHandler == nil {
		return nil
	}
	return b.InterceptHandler.NewUsageAccumulator(provider, model)
}

// ExtractResponseUsage resolves the adapter for host+path and invokes
// DetectResponseUsage. Implements platform.ResponseUsageDetector.
func (b *ConnectionBridge) ExtractResponseUsage(ctx context.Context, host, path string, resp *http.Response, body []byte) traffic.UsageMeta {
	if b.InterceptHandler == nil {
		return traffic.UsageMeta{Status: traffic.UsageStatusNonLLM}
	}
	return b.InterceptHandler.ExtractResponseUsage(ctx, host, path, resp, body)
}
