package clockoffset

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/secretstore"
)

// DriftLevel categorises the magnitude of the client<->server clock offset.
// The levels map to section 7.5 of the agent-auth architecture design doc:
// the agent never mutates the system clock; it records the observed offset
// and layers it over exp/iat comparisons in later tasks (3.7/3.8). The
// DriftLevel drives logging verbosity, the admin UI "clock anomaly" badge,
// and (for DriftError) an optional forced re-login to prevent a spoofed
// "1970" system clock from silently accepting expired tokens.
type DriftLevel int

const (
	// DriftNormal indicates |offset| < 5 min. No action beyond metrics.
	DriftNormal DriftLevel = iota
	// DriftInfo indicates 5 min <= |offset| < 15 min. INFO log + metric.
	DriftInfo
	// DriftWarn indicates 15 min <= |offset| < 1 h. WARN log + admin UI
	// "clock anomaly" badge on the device.
	DriftWarn
	// DriftError indicates |offset| >= 1 h. ERROR log + optional forced
	// re-login (prevents a 1970 system-clock exploit).
	DriftError
)

// String returns the lowercase label ("normal" / "info" / "warn" / "error")
// used in logs, metrics, and admin UI strings.
func (d DriftLevel) String() string {
	switch d {
	case DriftNormal:
		return "normal"
	case DriftInfo:
		return "info"
	case DriftWarn:
		return "warn"
	case DriftError:
		return "error"
	default:
		return fmt.Sprintf("unknown(%d)", int(d))
	}
}

// offsetStoreKey is the secretstore key under which the most recently
// observed server-time offset is persisted. Kept unexported so tests in
// this package can reference the schema without leaking it to callers.
const offsetStoreKey = "nexus.server_time_offset"

// ParseServerDate reads the HTTP Date header using the strict IMF-fixdate
// format (RFC 7231 section 7.1.1.1). Returns an error if the header is
// missing or unparseable.
func ParseServerDate(h http.Header) (time.Time, error) {
	raw := h.Get("Date")
	if raw == "" {
		return time.Time{}, errors.New("clockoffset: missing Date header")
	}
	t, err := time.Parse(http.TimeFormat, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("clockoffset: parse Date header %q: %w", raw, err)
	}
	return t, nil
}

// ComputeOffset returns serverTime - localTime. A positive result means the
// local clock is BEHIND the server; a negative result means the local clock
// is AHEAD of the server.
func ComputeOffset(server, local time.Time) time.Duration {
	return server.Sub(local)
}

// Classify returns the DriftLevel for the absolute value of the given
// offset. Boundaries use strict less-than, matching the design plan:
//
//	|offset| <  5 min -> DriftNormal
//	|offset| < 15 min -> DriftInfo
//	|offset| <  1 h   -> DriftWarn
//	otherwise         -> DriftError
//
// Exactly 5 min is DriftInfo, exactly 15 min is DriftWarn, and exactly 1 h
// is DriftError.
func Classify(offset time.Duration) DriftLevel {
	abs := offset
	if abs < 0 {
		abs = -abs
	}
	switch {
	case abs < 5*time.Minute:
		return DriftNormal
	case abs < 15*time.Minute:
		return DriftInfo
	case abs < time.Hour:
		return DriftWarn
	default:
		return DriftError
	}
}

// OffsetStore persists the most recently observed server-time offset as a
// signed int64 (nanoseconds, base-10 ASCII) in a secretstore.Store under
// the key "nexus.server_time_offset".
type OffsetStore struct {
	s secretstore.Store
}

// NewOffsetStore wraps a secretstore.Store. The caller retains ownership of
// the underlying Store and is responsible for closing it.
func NewOffsetStore(s secretstore.Store) *OffsetStore {
	return &OffsetStore{s: s}
}

// Load returns the last saved offset, or 0 when no offset has been saved
// yet or when the stored value is malformed. Malformed values are logged
// via slog.Default().Warn but do not return an error -- callers treat "no
// offset" and "bad stored offset" identically (fall back to the local
// clock). This keeps the agent bootable even after a corrupt write.
func (o *OffsetStore) Load() time.Duration {
	raw, err := o.s.Get(offsetStoreKey)
	if err != nil {
		if errors.Is(err, secretstore.ErrNotFound) {
			return 0
		}
		slog.Default().Warn("clockoffset: read offset store",
			slog.String("key", offsetStoreKey),
			slog.Any("err", err),
		)
		return 0
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		slog.Default().Warn("clockoffset: parse stored offset",
			slog.String("key", offsetStoreKey),
			slog.String("raw", string(raw)),
			slog.Any("err", err),
		)
		return 0
	}
	return time.Duration(n)
}

// Save persists the given offset. Returns the underlying secretstore error
// if the write fails.
func (o *OffsetStore) Save(d time.Duration) error {
	payload := strconv.FormatInt(d.Nanoseconds(), 10)
	if err := o.s.Set(offsetStoreKey, []byte(payload)); err != nil {
		return fmt.Errorf("clockoffset: save offset: %w", err)
	}
	return nil
}
