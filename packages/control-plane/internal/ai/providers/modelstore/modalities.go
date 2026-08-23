package modelstore

// defaultModalities derives a model's modality arrays from the other fields on
// the same row, for callers that supply none.
//
// The previous default was a flat ["text"] / ["text"] for every model, which
// contradicted the row it was written onto: an stt model landed declaring it
// accepts text rather than audio, an image model declared it emits text, and a
// model carrying features ["vision"] declared text-only input. That last one is
// not hypothetical — command-a-vision-07-2025 reached the production catalog
// advertising vision alongside inputModalities ["text"], and nothing downstream
// could reconcile the two.
//
// `type` answers which endpoint serves a model, and each endpoint implies the
// modality that endpoint moves, so a default that ignores it is choosing to be
// wrong where it had the answer. Callers that pass modalities explicitly keep
// them verbatim — this only fills a nil. The image question is not asked here:
// it arrives as the caller's `vision` feature and [FoldVision] settles it
// afterwards, on whatever array this returned.
func defaultModalities(modelType string) (in, out []string) {
	switch modelType {
	case "embedding":
		in, out = []string{"text"}, []string{"embedding"}
	case "image":
		in, out = []string{"text"}, []string{"image"}
	case "video":
		in, out = []string{"text"}, []string{"video"}
	case "tts":
		in, out = []string{"text"}, []string{"audio"}
	case "stt":
		in, out = []string{"audio"}, []string{"text"}
	case "realtime":
		in, out = []string{"text", "audio"}, []string{"text", "audio"}
	default:
		// chat, rerank, and anything not yet enumerated: text in, text out.
		in, out = []string{"text"}, []string{"text"}
	}
	return in, out
}

// FoldVision moves a caller's `vision` feature into the one place that fact
// lives, returning the features without it and the input modalities with
// "image".
//
// `vision` and `inputModalities ∋ image` are the same claim in two
// vocabularies, and two vocabularies for one fact is how they came to
// disagree: command-a-vision-07-2025 reached the production catalog
// advertising vision alongside inputModalities ["text"], and nothing
// downstream could reconcile them. The modality arrays win because they can
// express audio, video and files too, which `features` never could.
//
// Translated rather than rejected, and translated rather than dropped. The
// admin API is a shipped 1.0 contract: a client that has always sent
// features ["vision"] must keep working, and it must keep meaning what it
// meant. Dropping the string would silently discard the only statement such a
// client makes about images. Rejecting it would break them outright.
//
// Appended, never substituted — a vision model still accepts text. Replacing
// the array instead of extending it is the mistake that once left 94 catalog
// rows declaring image-only input.
// Exported because the update handler is the only place that can supply the
// base array — a PUT carrying only `features` needs the row's existing input
// modalities, and the store's single UPDATE statement never reads them.
func FoldVision(features, in []string) ([]string, []string) {
	kept := make([]string, 0, len(features))
	found := false
	for _, f := range features {
		if f == "vision" {
			found = true
			continue
		}
		kept = append(kept, f)
	}
	if !found {
		return features, in
	}
	if !contains(in, "image") {
		in = append(append(make([]string, 0, len(in)+1), in...), "image")
	}
	return kept, in
}

// contains reports whether xs holds v.
func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
