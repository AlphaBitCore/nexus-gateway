package proxy

import (
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
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

// collectRuleIDs returns the deduplicated SourceID list from a TransformSpan
// slice — used to populate the {redacted:true, ruleIds} placeholder when
// audit.Writer drops normalized content per storageAction=drop-content.
func collectRuleIDs(spans []normcore.TransformSpan) []string {
	if len(spans) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(spans))
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		if s.SourceID == "" {
			continue
		}
		if _, ok := seen[s.SourceID]; ok {
			continue
		}
		seen[s.SourceID] = struct{}{}
		out = append(out, s.SourceID)
	}
	return out
}
