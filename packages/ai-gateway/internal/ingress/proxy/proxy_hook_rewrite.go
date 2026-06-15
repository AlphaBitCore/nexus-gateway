package proxy

import (
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// contentBlocksToNormalized converts the hook pipeline's ModifiedContent
// (ordered []ContentBlock) into a traffic.NormalizedContent.Segments slice
// whose positions align with the ones emitted by the matching traffic
// adapter's ExtractRequest. Handlers extract request content via the
// format-aware traffic adapter (see Handler.trafficAdapterFor in
// traffic_adapter.go); this helper is the inverse join point that feeds
// hook-modified content back into the same adapter's RewriteRequestBody.
//
// Only text-type blocks contribute to segments: non-text blocks (images,
// tool_calls) were never in the extractor's output and therefore never
// consume a rewrite slot.
func contentBlocksToNormalized(blocks []hookcore.ContentBlock) traffic.NormalizedContent {
	segments := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type != "" && b.Type != "text" {
			continue
		}
		segments = append(segments, b.Text)
	}
	return traffic.NormalizedContent{Segments: segments}
}
