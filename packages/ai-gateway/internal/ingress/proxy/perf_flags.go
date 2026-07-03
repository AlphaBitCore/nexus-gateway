package proxy

import (
	"os"
	"sync"
)

// parallelHooksOnce gates the one-time read of the parallel-hooks perf flag.
var (
	parallelHooksOnce sync.Once
	parallelHooksFlag bool
)

// hookPrefilterOnce gates the one-time read of the request-hook prefilter flag.
var (
	hookPrefilterOnce sync.Once
	hookPrefilterFlag bool
)

// perfHookPrefilter reports whether the request-stage raw-body prefilter is
// active: when on (the default), a request whose body carries no JSON backslash
// escape and whose anchor-stripped superset scan finds nothing skips the
// structured content extraction (the dominant hooks-ON CPU cost) and runs the
// pipeline with a nil Normalized payload — provably detection-equivalent (see
// the prescan differential gate). It is ON by default because the optimization
// is the intended behaviour; NEXUS_HOOK_PREFILTER=0/false/off is the kill switch
// (instant revert to always-extract) and the A/B "control" arm.
func perfHookPrefilter() bool {
	hookPrefilterOnce.Do(func() {
		switch os.Getenv("NEXUS_HOOK_PREFILTER") {
		case "0", "false", "off", "no":
			hookPrefilterFlag = false
		default:
			hookPrefilterFlag = true
		}
	})
	return hookPrefilterFlag
}

// perfParallelHooks reports whether content-hook scans should run concurrently
// instead of sequentially. The rule-pack content hooks (pii-detector,
// content-safety, keyword-filter) are read-only scanners: Execute reads the
// extracted text segments and returns a decision + tags; it never mutates the
// shared HookInput between hooks (redaction is applied later at the audit
// storage-rewrite stage from the aggregated spans, not in the inter-hook
// pipeline). So scanning them concurrently is detection- and redaction-
// equivalent to the sequential pass — it only overlaps the per-hook cgo
// Vectorscan crossings, which is the dominant latency at sub-saturation load.
//
// Gated behind NEXUS_PARALLEL_HOOKS (default off) until the equivalence is
// confirmed by the redaction + differential test gates and a clean A/B.
func perfParallelHooks() bool {
	parallelHooksOnce.Do(func() {
		v := os.Getenv("NEXUS_PARALLEL_HOOKS")
		parallelHooksFlag = v == "1" || v == "true"
	})
	return parallelHooksFlag
}

// pureForward is the NEXUS_PERF_PURE_FORWARD benchmark switch. When on, the proxy
// skips its entire audit tail (no traffic_event) so throughput can be compared
// against forward-only gateways (Bifrost, agentgateway). Default off ("" or any
// value != "1") = audit stored, byte-identical to normal operation. Read once at
// package init; a plain var (not sync.Once) so tests can toggle it. Env-only by
// design — this must never be reachable via config/yaml/shadow, and must never be
// set in production (it disables the audit trail).
var pureForward = os.Getenv("NEXUS_PERF_PURE_FORWARD") == "1"

// PerfPureForward reports whether pure-forward benchmark mode is active. Exported
// for the wiring layer's startup WARN banner and self-identifying gauge.
func PerfPureForward() bool { return pureForward }
