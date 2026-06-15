package core

// NormalizedPayload.RedactedReason values.
const (
	// RedactedReasonOperatorDrop — the operator's storageAction was
	// drop-content; dropping the content is the configured policy.
	RedactedReasonOperatorDrop = "operator-drop"
	// RedactedReasonDegraded — the operator's storageAction was redact,
	// but the redaction could not be applied precisely to the stored copy,
	// so it degraded to the drop placeholder (never store what cannot be
	// redacted). RedactedDetail carries the cause.
	RedactedReasonDegraded = "redact-degraded"
)

// RedactionDegradeDetail.Cause values.
const (
	// DegradeCauseNoSpans — the hook demanded redaction but produced no
	// byte-addressed spans (keyword / content-safety matches locate no
	// byte ranges).
	DegradeCauseNoSpans = "no-spans"
	// DegradeCausePayloadUnmarshal — the stored normalized bytes did not
	// unmarshal into a NormalizedPayload, so spans could not be applied.
	DegradeCausePayloadUnmarshal = "payload-unmarshal"
	// DegradeCauseSpansUnresolved — one or more span content addresses did
	// not resolve on the storage-time payload (typically a cross-format
	// request where the hook-time projection indexes content differently),
	// and storage-time re-detection could not re-locate the content.
	DegradeCauseSpansUnresolved = "spans-unresolved"
	// DegradeCauseMarshalFailed — the redacted payload failed to re-marshal.
	DegradeCauseMarshalFailed = "marshal-failed"
)

// RedactionDegradeDetail diagnoses a redact→drop degradation on a stored
// NormalizedPayload. FailedAddresses lists ONLY the content addresses
// (e.g. "messages.2.content.0") of the spans that did not resolve — never
// the matched content itself.
type RedactionDegradeDetail struct {
	Cause           string   `json:"cause"`
	FailedAddresses []string `json:"failedAddresses,omitempty"`
}
