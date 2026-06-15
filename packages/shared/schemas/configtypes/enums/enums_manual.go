// Hand-maintained enums not derived from schema.prisma.
//
// BumpStatus is a TLS interception outcome enum used by the compliance proxy
// and agent audit paths. It is intentionally not a Prisma enum — the
// traffic_event.bump_status column stores it as a plain string so the data
// plane can emit values that are not yet known to the schema without forcing
// a migration in every release.

package enums

// BumpStatus is the outcome of a TLS interception ("bump") attempt.
type BumpStatus string

const (
	BumpStatusSuccess           BumpStatus = "BUMP_SUCCESS"
	BumpStatusFailedPassthrough BumpStatus = "BUMP_FAILED_PASSTHROUGH"
	BumpStatusExemptConfigured  BumpStatus = "BUMP_EXEMPT_CONFIGURED"
	BumpStatusExemptPinned      BumpStatus = "BUMP_EXEMPT_PINNED"
	BumpStatusDisabledEmergency BumpStatus = "BUMP_DISABLED_EMERGENCY"
)
