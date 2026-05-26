package policy

// Override is the nullable, per-resource view of Policy. Each field is a
// pointer so a NULL column on `interception_domain` / `Provider` falls
// through to the global default during merge. Resolvers read DB columns
// into one of these and call `Resolve(global, override)`.
type Override struct {
	Mode                *Mode
	ChunkBytes          *int
	HookTimeoutMs       *int
	MaxBufferBytes      *int
	FailBehavior        *FailBehavior
	CaptureRequestBody  *bool
	CaptureResponseBody *bool
	RawSpillEnabled     *bool
}

// Resolve merges `override` over `globalDefault`. A nil `override` means
// "no per-resource override row" and returns the global default unchanged.
// Invalid fields on either side are surfaced via `Policy.IsValid()` after
// merging — Resolve does not silently sanitize.
func Resolve(globalDefault Policy, override *Override) Policy {
	merged := globalDefault
	if override == nil {
		return merged
	}
	if override.Mode != nil {
		merged.Mode = *override.Mode
	}
	if override.ChunkBytes != nil {
		merged.ChunkBytes = *override.ChunkBytes
	}
	if override.HookTimeoutMs != nil {
		merged.HookTimeoutMs = *override.HookTimeoutMs
	}
	if override.MaxBufferBytes != nil {
		merged.MaxBufferBytes = *override.MaxBufferBytes
	}
	if override.FailBehavior != nil {
		merged.FailBehavior = *override.FailBehavior
	}
	if override.CaptureRequestBody != nil {
		merged.CaptureRequestBody = *override.CaptureRequestBody
	}
	if override.CaptureResponseBody != nil {
		merged.CaptureResponseBody = *override.CaptureResponseBody
	}
	if override.RawSpillEnabled != nil {
		merged.RawSpillEnabled = *override.RawSpillEnabled
	}
	return merged
}
