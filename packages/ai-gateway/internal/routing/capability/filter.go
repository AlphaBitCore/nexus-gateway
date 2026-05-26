package capability

// Compatible reports whether the given embedding request is compatible with
// the model's capability descriptor. Returns ok=true with empty reason on
// match; ok=false with a human-readable reason and a CandidateCapability
// projection on mismatch.
//
// Rules:
//   - cap == nil → reject (no capability data means not an embedding model)
//   - cap.Embeddings == nil → reject (capability JSON exists but has no embeddings block)
//   - req.Dimensions != nil → must appear in cap.Embeddings.SupportedDimensions
//     (when SupportedDimensions is empty/nil, the model rejects any dimensions parameter)
//   - req.BatchSize > cap.Embeddings.MaxBatchSize → reject (when MaxBatchSize > 0)
//   - req.EncodingFormat != "" → must appear in cap.Embeddings.SupportedEncodingFormats
//     (defaulting to ["float","base64"] when omitted from the descriptor)
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
		MaxBatchSize:             emb.MaxBatchSize,
		SupportedEncodingFormats: effectiveEncodingFormats(emb),
		// Required extensions advertised by the model descriptor itself
		// (admin-declared). Rule 4 / Rule 5 below overwrite this with the
		// specific unmet extension on a rejection so the failure message
		// names exactly which extension was missing.
		RequiredExtensions: emb.RequiredExtensions,
	}

	// Rule 1: dimensions parameter
	if req != nil && req.Dimensions != nil {
		d := *req.Dimensions
		if !containsInt(emb.SupportedDimensions, d) {
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

// effectiveEncodingFormats returns the model's supported encoding formats.
// When the descriptor omits SupportedEncodingFormats, the OpenAI-compatible
// default ["float","base64"] applies.
func effectiveEncodingFormats(emb *EmbeddingsCapability) []string {
	if len(emb.SupportedEncodingFormats) > 0 {
		return emb.SupportedEncodingFormats
	}
	return []string{"float", "base64"}
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
