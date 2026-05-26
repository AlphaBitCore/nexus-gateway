package executor

import (
	"errors"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	cfgpolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
)

type errClass int

const (
	classSuccess errClass = iota
	classNoFailoverNoRetry
	classNetwork
	classTimeout
	classRate429
	class5xx
)

// classify maps an adapter.Execute outcome to (errClass, cfgpolicy.ErrorClass).
// The first return is the executor's branching key; the second is the
// matching cfgpolicy.ErrorClass for RetryOn membership checks (empty
// when not retryable).
func classify(resp *provcore.Response, err error) (errClass, cfgpolicy.ErrorClass) {
	if err == nil && resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return classSuccess, ""
	}

	var pe *provcore.ProviderError
	if errors.As(err, &pe) {
		switch pe.Code {
		case provcore.CodeRateLimited:
			return classRate429, cfgpolicy.ErrorClassRate429
		case provcore.CodeTimeout:
			return classTimeout, cfgpolicy.ErrorClassTimeout
		case provcore.CodeUpstreamError:
			return class5xx, cfgpolicy.ErrorClass5xx
		case provcore.CodeInvalidRequest,
			provcore.CodeAuthFailed,
			provcore.CodeEndpointUnsupported,
			provcore.CodeNotImplemented,
			provcore.CodeNoCompatibleProvider:
			return classNoFailoverNoRetry, ""
		}
		return classNetwork, cfgpolicy.ErrorClassNetwork
	}

	if err != nil {
		return classNetwork, cfgpolicy.ErrorClassNetwork
	}

	return classNetwork, cfgpolicy.ErrorClassNetwork
}
