package executor

import (
	"errors"
	"io"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	configtypes "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name      string
		resp      *provcore.Response
		err       error
		wantClass errClass
		wantErrCl configtypes.ErrorClass
	}{
		{"2xx success", &provcore.Response{StatusCode: 200}, nil, classSuccess, ""},
		{"rate limited", nil, &provcore.ProviderError{Code: provcore.CodeRateLimited, Status: 429}, classRate429, configtypes.ErrorClassRate429},
		{"timeout", nil, &provcore.ProviderError{Code: provcore.CodeTimeout, Status: 504}, classTimeout, configtypes.ErrorClassTimeout},
		{"upstream 5xx", nil, &provcore.ProviderError{Code: provcore.CodeUpstreamError, Status: 502}, class5xx, configtypes.ErrorClass5xx},
		{"invalid request 4xx", nil, &provcore.ProviderError{Code: provcore.CodeInvalidRequest, Status: 400}, classNoFailoverNoRetry, ""},
		{"auth failed", nil, &provcore.ProviderError{Code: provcore.CodeAuthFailed, Status: 401}, classNoFailoverNoRetry, ""},
		{"endpoint unsupported", nil, &provcore.ProviderError{Code: provcore.CodeEndpointUnsupported, Status: 400}, classNoFailoverNoRetry, ""},
		{"not implemented", nil, &provcore.ProviderError{Code: provcore.CodeNotImplemented, Status: 501}, classNoFailoverNoRetry, ""},
		{"no compatible provider", nil, &provcore.ProviderError{Code: provcore.CodeNoCompatibleProvider, Status: 400}, classNoFailoverNoRetry, ""},
		{"plain transport error EOF", nil, io.EOF, classNetwork, configtypes.ErrorClassNetwork},
		{"plain transport error generic", nil, errors.New("dial tcp: connection refused"), classNetwork, configtypes.ErrorClassNetwork},
		{"unknown ProviderError code", nil, &provcore.ProviderError{Code: "totally_made_up", Status: 599}, classNetwork, configtypes.ErrorClassNetwork},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotClass, gotErrCl := classify(tc.resp, tc.err)
			if gotClass != tc.wantClass {
				t.Errorf("class: got %v want %v", gotClass, tc.wantClass)
			}
			if gotErrCl != tc.wantErrCl {
				t.Errorf("errClass: got %q want %q", gotErrCl, tc.wantErrCl)
			}
		})
	}
}
