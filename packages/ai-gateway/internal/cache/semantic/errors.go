package semantic

import "errors"

// Typed errors returned by Client methods.
var (
	// ErrIndexExists is returned by EnsureIndex when FT.CREATE reports
	// that the index already exists. The caller should treat this as a
	// no-op success.
	ErrIndexExists = errors.New("semantic/client: index already exists")

	// ErrIndexMissing is returned by DropIndex when FT.DROPINDEX reports
	// that the named index does not exist. The caller should treat this as
	// a no-op success (idempotent drop).
	ErrIndexMissing = errors.New("semantic/client: index does not exist")

	// ErrEntryTooLarge is returned by StoreEntry when the serialised entry
	// size exceeds maxEntryBytes. The write is skipped; the L1 write is
	// unaffected.
	ErrEntryTooLarge = errors.New("semantic/client: entry exceeds max entry size")

	// ErrValkeyUnavailable is returned when the Valkey connection is
	// unavailable or returns a network-level error. Callers stamp
	// GatewayCacheSkipReasonValkeyUnavailable.
	ErrValkeyUnavailable = errors.New("semantic/client: Valkey unavailable")

	// ErrSearchUnavailable is returned when FT.SEARCH fails for a reason
	// other than a connection error (e.g., index was dropped between the
	// EnsureIndex call and the query). Callers stamp
	// GatewayCacheSkipReasonSemanticSearchError.
	ErrSearchUnavailable = errors.New("semantic/client: search unavailable")
)
