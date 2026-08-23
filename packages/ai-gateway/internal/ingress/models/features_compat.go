package models

import "slices"

// visionCompatFeature is the legacy spelling of "this model accepts images".
//
// The fact now lives in inputModalities, which is the only vocabulary that can
// also express audio, video and files — and the only one the router consults.
// It is no longer stored on the row. But /v1/models is a shipped 1.0 contract
// and SDK callers have been reading `features` since before the modality
// arrays existed, so the string is put back on the way out.
//
// Derived, never stored: a column and a derivation for the same fact is how
// they came to disagree on 34 production rows, one of which advertised vision
// beside inputModalities ["text"].
const visionCompatFeature = "vision"

// withDerivedFeatures returns the outward feature list: what the row stores,
// plus the compatibility aliases derivable from its modalities.
//
// Chat only. An embeddings model that accepts images is doing something the
// word "vision" has never meant to a chat SDK — gemini-embedding-2 is the one
// catalog row where the distinction is observable, and calling it a vision
// model would be a new claim, not a preserved one.
func withDerivedFeatures(modelType string, features, inputModalities []string) []string {
	if modelType != "chat" || !slices.Contains(inputModalities, "image") {
		return features
	}
	if slices.Contains(features, visionCompatFeature) {
		return features
	}
	// Copied rather than appended in place: `features` is the store row's
	// slice, shared by every entry built from the same cached catalog.
	out := make([]string, 0, len(features)+1)
	out = append(out, features...)
	return append(out, visionCompatFeature)
}
