package capability

// Compatible reports whether the given embedding request is compatible with
// the model's capability descriptor. Returns ok=true with empty reason on
// match; ok=false with a human-readable reason and a CandidateCapability
// projection on mismatch.
//
// Rules:
//   - cap == nil → reject (no capability data means not an embedding model)
//   - cap.Embeddings == nil → reject (capability JSON exists but has no embeddings block)
//   - req.Dimensions != nil → when the model declares a range (MaxDimension > 0)
//     it must fall inside [MinDimension or 1, MaxDimension]; otherwise it must
//     appear in cap.Embeddings.SupportedDimensions (and when that list is
//     empty/nil too, the model rejects any dimensions parameter)
//   - req.BatchSize > cap.Embeddings.MaxBatchSize → reject (when MaxBatchSize > 0)
//   - req.EncodingFormat != "" → must appear in cap.Embeddings.SupportedEncodingFormats
//     (defaulting to ["float"] when omitted from the descriptor — base64 must be
//     explicitly opted-in because only some provider codecs re-encode to it)
//   - req.InputType != "" → must appear in cap.Embeddings.SupportedInputTypes (Cohere)
//   - req.TaskType != "" → must appear in cap.Embeddings.SupportedTaskTypes (Gemini)
func Compatible(req *EmbeddingRequest, cap *ModelCapability) (ok bool, reason string, candidate CandidateCapability) {
	if cap == nil {
		return false, "model has no capability data", CandidateCapability{}
	}
	if cap.Embeddings == nil {
		return false, "model capability descriptor has no embeddings block", CandidateCapability{}
	}
	emb := cap.Embeddings

	proj := CandidateCapability{
		SupportedDimensions:      emb.SupportedDimensions,
		MinDimension:             emb.MinDimension,
		MaxDimension:             emb.MaxDimension,
		MaxBatchSize:             emb.MaxBatchSize,
		SupportedEncodingFormats: effectiveEncodingFormats(emb),
		// Required extensions advertised by the model descriptor itself
		// (admin-declared). Rule 4 / Rule 5 below overwrite this with the
		// specific unmet extension on a rejection so the failure message
		// names exactly which extension was missing.
		RequiredExtensions: emb.RequiredExtensions,
	}

	// Rule 1: dimensions parameter.
	//
	// A declared range wins over the enumeration. A Matryoshka model accepts
	// any dimension up to its maximum, so the honest description is the bound,
	// and anything inside it is forwarded for the provider to judge. Only when
	// no range is declared does the fixed list apply — that list is still the
	// right description for a model that really does emit one size.
	if req != nil && req.Dimensions != nil {
		d := *req.Dimensions
		switch {
		case emb.MaxDimension > 0:
			min := emb.MinDimension
			if min <= 0 {
				min = 1
			}
			if d < min || d > emb.MaxDimension {
				return false, "requested dimensions outside the range this model supports", proj
			}
		case !containsInt(emb.SupportedDimensions, d):
			return false, "requested dimensions not supported by this model", proj
		}
	}

	// Rule 2: batch size
	if req != nil && emb.MaxBatchSize > 0 && req.BatchSize > emb.MaxBatchSize {
		return false, "batch size exceeds model maximum", proj
	}

	// Rule 3: encoding format
	if req != nil && req.EncodingFormat != "" {
		ef := effectiveEncodingFormats(emb)
		if !containsStr(ef, req.EncodingFormat) {
			return false, "requested encoding_format not supported by this model", proj
		}
	}

	// Rule 4: Cohere input_type
	if req != nil && req.InputType != "" {
		if !containsStr(emb.SupportedInputTypes, req.InputType) {
			reqExt := []string{"nexus.ext.cohere.input_type=" + req.InputType}
			proj.RequiredExtensions = reqExt
			return false, "requested Cohere input_type not supported by this model", proj
		}
	}

	// Rule 5: Gemini task type
	if req != nil && req.TaskType != "" {
		if !containsStr(emb.SupportedTaskTypes, req.TaskType) {
			reqExt := []string{"nexus.ext.gemini.taskType=" + req.TaskType}
			proj.RequiredExtensions = reqExt
			return false, "requested Gemini taskType not supported by this model", proj
		}
	}

	return true, "", proj
}

// effectiveEncodingFormats returns the encoding formats a request may ask for.
//
// Both "float" and "base64" are always available, whatever the descriptor says,
// because neither depends on the provider wire any more: "float" is what every
// embedding codec emits unconditionally, and "base64" is guaranteed by the
// ingress response layer (honorEmbeddingEncodingFormat in
// internal/ingress/proxy), which re-encodes canonical float vectors to
// little-endian float32 base64 for the caller.
//
// This used to require an explicit per-model "base64" declaration, on the
// reasoning that only the OpenAI-native codec passed encoding_format to the wire
// and a base64 request would otherwise be silently downgraded (or 400'd by
// Cohere). That reasoning was sound but the guard did not hold: the
// explicit-model passthrough path never runs this filter, so a base64 request to
// e.g. gemini-embedding-001 sailed through and WAS silently downgraded — and the
// OpenAI SDKs, having implicitly asked for base64, decoded the float array into a
// quarter-length garbage vector (observed on staging 2026-07-27). Guaranteeing
// base64 on the response path fixes the whole class instead of gating it.
//
// A descriptor that declares formats still widens the set (a provider-specific
// encoding beyond these two), it just cannot narrow it below the two the gateway
// itself guarantees.
func effectiveEncodingFormats(emb *EmbeddingsCapability) []string {
	out := []string{"float", "base64"}
	for _, f := range emb.SupportedEncodingFormats {
		if f != "float" && f != "base64" {
			out = append(out, f)
		}
	}
	return out
}

func containsInt(slice []int, v int) bool {
	for _, x := range slice {
		if x == v {
			return true
		}
	}
	return false
}

func containsStr(slice []string, v string) bool {
	for _, x := range slice {
		if x == v {
			return true
		}
	}
	return false
}
