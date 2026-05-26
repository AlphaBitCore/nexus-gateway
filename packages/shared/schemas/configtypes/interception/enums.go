// Hand-maintained Go mirror of the corresponding schema.prisma model. Keep in lockstep with schema changes — see docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md §5.

package interception

// DefaultPathAction -- generated from schema.prisma enum.
type DefaultPathAction string

const (
	DefaultPathActionProcess     DefaultPathAction = "PROCESS"
	DefaultPathActionPassthrough DefaultPathAction = "PASSTHROUGH"
	DefaultPathActionBlock       DefaultPathAction = "BLOCK"
)

// FailureAction -- generated from schema.prisma enum.
type FailureAction string

const (
	FailureActionFailOpen   FailureAction = "FAIL_OPEN"
	FailureActionFailClosed FailureAction = "FAIL_CLOSED"
)

// HostMatchType -- generated from schema.prisma enum.
type HostMatchType string

const (
	HostMatchTypeExact  HostMatchType = "EXACT"
	HostMatchTypePrefix HostMatchType = "PREFIX"
	HostMatchTypeGlob   HostMatchType = "GLOB"
	HostMatchTypeRegex  HostMatchType = "REGEX"
)

// NetworkZone -- generated from schema.prisma enum.
type NetworkZone string

const (
	NetworkZonePublic   NetworkZone = "PUBLIC"
	NetworkZoneInternal NetworkZone = "INTERNAL"
)

// PathAction -- generated from schema.prisma enum.
type PathAction string

const (
	PathActionProcess     PathAction = "PROCESS"
	PathActionPassthrough PathAction = "PASSTHROUGH"
	PathActionBlock       PathAction = "BLOCK"
)

// PathMatchType -- generated from schema.prisma enum.
type PathMatchType string

const (
	PathMatchTypeExact  PathMatchType = "EXACT"
	PathMatchTypePrefix PathMatchType = "PREFIX"
	PathMatchTypeGlob   PathMatchType = "GLOB"
	PathMatchTypeRegex  PathMatchType = "REGEX"
)
