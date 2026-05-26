package typology

// KindFromPathSegment maps a path-segment string (the trimmed suffix
// after the API-version prefix, e.g. "chat/completions", "embeddings",
// "audio/transcriptions") to its canonical EndpointKind. Returns the
// empty EndpointKind ("") for unknown segments; callers that have not
// yet classified the endpoint treat empty as "applies to all" (the
// backward-compatible default the hook pipeline relies on).
//
// This is the single source of truth for path-segment → EndpointKind;
// shared/policy/hooks/core.EndpointTypeFromPath and
// ai-gateway/internal/platform/audit.EndpointTypeFromPath both delegate
// here. Adding a new segment requires updating only this table (and the
// matching ClassifyPath rule in defaults.go).
func KindFromPathSegment(segment string) EndpointKind {
	switch segment {
	case "chat/completions", "completions", "responses":
		return EndpointKindChat
	case "embeddings":
		return EndpointKindEmbeddings
	case "audio/transcriptions", "audio/translations":
		return EndpointKindSTT
	case "audio/speech":
		return EndpointKindTTS
	case "images/generations", "images/edits", "images/variations":
		return EndpointKindImageGeneration
	case "batches":
		return EndpointKindBatch
	}
	return ""
}
