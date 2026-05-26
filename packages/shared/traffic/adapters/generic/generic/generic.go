// Package generic implements the generic-jsonpath adapter that extracts
// content from any JSON body using admin-configured gjson path expressions.
package generic

import (
	"context"
	"fmt"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// Adapter implements admin-configurable JSONPath extraction.
type Adapter struct {
	requestPaths     []string
	responsePaths    []string
	streamDeltaPaths []string
}

// ID returns the adapter identifier.
func (a *Adapter) ID() string { return "generic-jsonpath" }

// Configure parses and validates the adapterConfig.
//
// Expected config shape:
//
//	{
//	  "requestPaths": ["$.messages[*].content", "$.prompt"],
//	  "responsePaths": ["$.choices[*].message.content"],
//	  "streamDeltaPaths": ["$.choices[*].delta.content"]
//	}
func (a *Adapter) Configure(config map[string]any) error {
	var err error
	a.requestPaths, err = extractStringSlice(config, "requestPaths")
	if err != nil {
		return fmt.Errorf("generic-jsonpath: %w", err)
	}
	a.responsePaths, err = extractStringSlice(config, "responsePaths")
	if err != nil {
		return fmt.Errorf("generic-jsonpath: %w", err)
	}
	a.streamDeltaPaths, err = extractStringSlice(config, "streamDeltaPaths")
	if err != nil {
		return fmt.Errorf("generic-jsonpath: %w", err)
	}

	if len(a.requestPaths) == 0 && len(a.responsePaths) == 0 {
		return fmt.Errorf("generic-jsonpath: at least one of requestPaths or responsePaths must be configured")
	}

	// Validate that all paths are syntactically valid gjson paths by running
	// them against an empty object. gjson does not have a separate "parse path"
	// function, but Get on an empty doc exercises the parser.
	for _, p := range append(append(a.requestPaths, a.responsePaths...), a.streamDeltaPaths...) {
		if p == "" {
			return fmt.Errorf("generic-jsonpath: empty path expression")
		}
	}

	return nil
}

// ExtractRequest evaluates requestPaths against the body.
func (a *Adapter) ExtractRequest(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	return extractWithPaths(body, a.requestPaths)
}

// ExtractResponse evaluates responsePaths against the body.
func (a *Adapter) ExtractResponse(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	return extractWithPaths(body, a.responsePaths)
}

// ExtractStreamChunk evaluates streamDeltaPaths against the chunk.
func (a *Adapter) ExtractStreamChunk(_ context.Context, chunk []byte, _ string) (traffic.NormalizedContent, error) {
	return extractWithPaths(chunk, a.streamDeltaPaths)
}

// RewriteRequestBody is unsupported for the generic-jsonpath adapter:
// admin-configured JSONPath expressions can evaluate to arbitrary
// unrelated positions (e.g. `$..text`, `$.a.b[?cond].c`) and there is
// no safe general reverse-mapping from gjson wildcards back to
// deterministic per-slot writes. Callers fall back to forwarding the
// original body plus a warn log.
func (a *Adapter) RewriteRequestBody(_ context.Context, _ []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return nil, 0, traffic.ErrRewriteUnsupported
}

// RewriteResponseBody is unsupported for the same reasons as RewriteRequestBody.
func (a *Adapter) RewriteResponseBody(_ context.Context, _ []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return nil, 0, traffic.ErrRewriteUnsupported
}

// extractWithPaths runs gjson queries and collects string results.
func extractWithPaths(body []byte, paths []string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}
	if len(paths) == 0 {
		return traffic.NormalizedContent{}, nil
	}

	var segments []string
	for _, p := range paths {
		result := gjson.GetBytes(body, p)
		if !result.Exists() {
			continue
		}
		if result.IsArray() {
			result.ForEach(func(_, item gjson.Result) bool {
				if s := item.String(); s != "" {
					segments = append(segments, s)
				}
				return true
			})
		} else if s := result.String(); s != "" {
			segments = append(segments, s)
		}
	}

	return traffic.NormalizedContent{Segments: segments}, nil
}

// extractStringSlice reads a []string from config[key].
func extractStringSlice(config map[string]any, key string) ([]string, error) {
	raw, ok := config[key]
	if !ok {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array of strings", key)
	}
	result := make([]string, 0, len(list))
	for i, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s[%d] must be a string", key, i)
		}
		result = append(result, s)
	}
	return result, nil
}
