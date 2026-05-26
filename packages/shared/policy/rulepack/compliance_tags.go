package rulepack

// Canonical tag constants. Starter packs reference these exact strings in
// their labels list. Renaming any of these is a breaking change: audit
// tooling, analytics queries, and customer dashboards pin to these
// literals.
//
// Format: "<namespace>:<value>" lowercase kebab after the colon.
const (
	TagDetectorPromptInjection = "detector:prompt-injection"
	TagDetectorJailbreak       = "detector:jailbreak"
	TagDetectorSecretLeak      = "detector:secret-leak"
	TagDetectorToolCallSafety  = "detector:tool-call-safety"
	TagDetectorContentSafety   = "detector:content-safety"

	TagSeverityPublic       = "severity:public"
	TagSeverityInternal     = "severity:internal"
	TagSeverityConfidential = "severity:confidential"
	TagSeverityRestricted   = "severity:restricted"
)
