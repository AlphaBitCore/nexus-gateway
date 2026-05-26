package validators

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// RequestSizeValidator enforces maximum request body size limits.
// Applies to all endpoints and modalities via AnyEndpointAnyModality.
type RequestSizeValidator struct {
	core.AnyEndpointAnyModality
	cfg                 *core.HookConfig
	maxSizeBytes        int64
	excludeContentTypes map[string]bool
}

// NewRequestSizeValidator constructs a RequestSizeValidator from declarative config.
//
// Expected config shape:
//
//	{
//	  "maxSizeBytes": 10485760,
//	  "excludeContentTypes": ["multipart/form-data"]
//	}
func NewRequestSizeValidator(cfg *core.HookConfig) (core.Hook, error) {
	maxSize, ok := core.ToInt64(cfg.Config["maxSizeBytes"])
	if !ok || maxSize <= 0 {
		return nil, fmt.Errorf("request-size-validator: 'maxSizeBytes' must be a positive integer")
	}

	excludeMap := make(map[string]bool)
	if rawExclude, ok := cfg.Config["excludeContentTypes"]; ok {
		list, ok := rawExclude.([]any)
		if !ok {
			return nil, fmt.Errorf("request-size-validator: 'excludeContentTypes' must be an array")
		}
		for _, raw := range list {
			ct, _ := raw.(string)
			if ct != "" {
				excludeMap[strings.ToLower(ct)] = true
			}
		}
	}

	return &RequestSizeValidator{
		cfg:                 cfg,
		maxSizeBytes:        maxSize,
		excludeContentTypes: excludeMap,
	}, nil
}

// Execute checks the input body size against the configured limit.
func (rsv *RequestSizeValidator) Execute(_ context.Context, input *core.HookInput) (*core.HookResult, error) {
	start := time.Now()

	result := &core.HookResult{
		HookID:           rsv.cfg.ID,
		ImplementationID: rsv.cfg.ImplementationID,
		HookName:         rsv.cfg.Name,
	}

	// Check if this content type is excluded from size validation.
	ct := strings.ToLower(input.ContentType)
	// Match on the base content type (before any parameters like charset).
	baseCT := ct
	if idx := strings.IndexByte(ct, ';'); idx >= 0 {
		baseCT = strings.TrimSpace(ct[:idx])
	}
	if rsv.excludeContentTypes[baseCT] {
		result.Decision = core.Approve
		result.LatencyMs = int(time.Since(start).Milliseconds())
		return result, nil
	}

	if input.BodySize > rsv.maxSizeBytes {
		result.Decision = core.RejectHard
		result.Reason = fmt.Sprintf("request body size %d exceeds limit %d bytes", input.BodySize, rsv.maxSizeBytes)
		result.ReasonCode = "REQUEST_TOO_LARGE"
		result.LatencyMs = int(time.Since(start).Milliseconds())
		return result, nil
	}

	result.Decision = core.Approve
	result.LatencyMs = int(time.Since(start).Milliseconds())
	return result, nil
}
