package pipeline

// warnUnknownImpl logs a warning for an implementationId that is advertised in
// the database but has no factory registered. The warning fires at most once
// per unique implementationId per reload epoch — Swap() resets the dedup set,
// so a subsequent reload that still references an unknown id will log again.
// The hookId / hookName of the first-seen row are included so operators can
// locate the offending row without searching.
func (r *PolicyResolver) warnUnknownImpl(implID, hookID, hookName string) {
	r.warnedMu.Lock()
	if _, seen := r.warnedUnknown[implID]; seen {
		r.warnedMu.Unlock()
		return
	}
	if r.warnedUnknown == nil {
		r.warnedUnknown = make(map[string]struct{})
	}
	r.warnedUnknown[implID] = struct{}{}
	r.warnedMu.Unlock()

	r.logger.Warn("unknown hook implementation, skipping",
		"implementationId", implID,
		"hookId", hookID,
		"hookName", hookName,
	)
}

// warnSkippedHook logs that a hook was skipped during pipeline build because
// its factory failed or it was stage-incompatible. Deduplicated per hookId
// per reload epoch (Swap resets the dedup set) so a persistently-broken hook
// logs once per reload instead of once per resolve() call. This is the
// availability-first degradation path: the offending hook is dropped; the
// rest of the pipeline still builds and runs.
func (r *PolicyResolver) warnSkippedHook(implID, hookID, hookName string, cause error) {
	r.warnedMu.Lock()
	dedupKey := "skip:" + hookID
	if _, seen := r.warnedUnknown[dedupKey]; seen {
		r.warnedMu.Unlock()
		return
	}
	if r.warnedUnknown == nil {
		r.warnedUnknown = make(map[string]struct{})
	}
	r.warnedUnknown[dedupKey] = struct{}{}
	r.warnedMu.Unlock()

	r.logger.Warn("compliance hook skipped during pipeline build (degrading to this hook off)",
		"implementationId", implID,
		"hookId", hookID,
		"hookName", hookName,
		"error", cause,
	)
}
