package audit

// filterHookStage returns a fresh slice of HookExecRecord rows whose Stage
// matches one of `stages`. Used by recordToMessage to split the combined
// rec.HooksPipeline into the dual `request_hooks_pipeline` /
// `response_hooks_pipeline` columns on the wire.
func filterHookStage(in []HookExecRecord, stages ...string) []HookExecRecord {
	if len(in) == 0 || len(stages) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(stages))
	for _, s := range stages {
		want[s] = struct{}{}
	}
	out := make([]HookExecRecord, 0, len(in))
	for _, r := range in {
		if _, ok := want[r.Stage]; ok {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// nilIfEmpty returns nil for an empty string and a pointer to s otherwise.
// Used by recordToMessage to map zero-value fields to SQL NULL.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// firstNonNil returns the first non-nil int pointer from the arguments.
// Used by recordToMessage to prefer an explicit Record field when set and
// fall back to a derived aggregate otherwise (e.g. RequestHooksMs derived
// from HooksPipeline if the proxy handler hasn't set it explicitly).
func firstNonNil(ps ...*int) *int {
	for _, p := range ps {
		if p != nil {
			return p
		}
	}
	return nil
}

// sumHookLatenciesMs returns the aggregate per-hook latency (ms) for the
// hook rows whose Stage matches one of `stages`. Returns nil when no hook
// in the requested stages ran — distinguished from zero so the resulting
// `request_hooks_ms` / `response_hooks_ms` columns stay NULL for bypass /
// no-hook requests (P95 queries should not count those as 0ms).
//
// Used by recordToMessage to populate the hook-aggregate columns from
// the existing per-hook latency data already in rec.HooksPipeline.
func sumHookLatenciesMs(in []HookExecRecord, stages ...string) *int {
	if len(in) == 0 || len(stages) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(stages))
	for _, s := range stages {
		want[s] = struct{}{}
	}
	var (
		total int
		ran   bool
	)
	for _, r := range in {
		if _, ok := want[r.Stage]; !ok {
			continue
		}
		ran = true
		if r.LatencyMs > 0 {
			total += r.LatencyMs
		}
	}
	if !ran {
		return nil
	}
	return &total
}

// firstNonEmptyStr returns a if non-empty, else b. Used for
// target_method / target_path stamping to fall back to the request-side
// value when the gateway didn't set a distinct target (transparent path
// or no cross-format routing).
func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
