package alerting

import (
	"fmt"
	"github.com/goccy/go-json"
	"strings"
	"time"
)

// Severity classifies the impact level of an alert.
//
// Canonical wire / Go form is lowercase ("critical" / "high" / "medium" /
// "low" / "info") — chosen because Go constants, JSON, metric labels, and
// log chips all already use lowercase. The Prisma `AlertSeverity` enum on
// disk uses uppercase ("CRITICAL", ...) for operator-friendly SQL; the
// `dbSeverity` / `goSeverity` helpers in store.go translate at the DB
// boundary so the rest of the system only ever sees lowercase values.
//
// The Parse / ParseLoose / IsValid / MarshalJSON / UnmarshalJSON methods
// below make Severity a proper typed enum: callers cannot smuggle an
// arbitrary string through the type — every boundary (admin POST body,
// query param, DB scan) funnels through one of the parsers and a typo or
// drifted value is rejected with a useful error.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
	SeverityInfo     Severity = "info"
)

// AllSeverities is the canonical, ordered list of valid Severity values
// (highest impact first). Used by handlers / tests / docs / UI sync — do
// not iterate over a map literal because Go map iteration order is random.
var AllSeverities = []Severity{
	SeverityCritical,
	SeverityHigh,
	SeverityMedium,
	SeverityLow,
	SeverityInfo,
}

// AllSourceTypes is the canonical set of AlertRule.sourceType values.
// SourceType is intentionally still a `string` field on Rule (the values are
// stable enough that a typed enum would be over-engineering), but every
// value in BuiltinRules must be a member of this set and the Prisma schema
// doc-comment on AlertRule.sourceType must match it exactly.
var AllSourceTypes = []string{
	"quota",
	"proxy",
	"thing",
	"provider",
	"auth",
	"system",
}

// String implements fmt.Stringer. Always returns the lowercase canonical
// form even if the underlying value was constructed via an unchecked cast.
func (s Severity) String() string { return strings.ToLower(string(s)) }

// IsValid reports whether s is one of the five recognised severity values.
// It is case-sensitive against the lowercase canonical form — callers that
// accept upstream input (HTTP body, query string, DB enum) should normalise
// via Parse / ParseLoose first.
func (s Severity) IsValid() bool {
	switch s {
	case SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow, SeverityInfo:
		return true
	}
	return false
}

// Parse converts a raw string into a Severity, requiring the canonical
// lowercase form. Unknown values return an error naming the offending
// input and the valid set. Use this for admin POST bodies where strict
// validation is desired.
func Parse(s string) (Severity, error) {
	sev := Severity(s)
	if !sev.IsValid() {
		return "", fmt.Errorf("invalid severity %q: must be one of critical, high, medium, low, info", s)
	}
	return sev, nil
}

// ParseLoose is the case-insensitive Parse used at boundaries we do not
// fully control: the Prisma `AlertSeverity` enum returns uppercase
// ("CRITICAL"); legacy AlertChannel.severities rows may carry mixed-case
// strings written before the typed enum landed. The Prisma DB-boundary
// scanner in store.go is the canonical caller.
func ParseLoose(s string) (Severity, error) {
	return Parse(strings.ToLower(s))
}

// MarshalJSON emits the canonical lowercase form so admin API responses
// and audit JSON stay stable regardless of how the value was constructed.
func (s Severity) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// UnmarshalJSON parses a JSON string and rejects unknown severities. This
// makes Severity-typed fields on admin request bodies (e.g. Channel.severities,
// updateRuleBody.DefaultSeverity) fail validation at decode time rather than
// silently leaking a typo into the DB.
func (s *Severity) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("severity: expected JSON string: %w", err)
	}
	parsed, err := Parse(raw)
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// ParseSeverityList parses a slice of strings into Severity values using
// strict Parse semantics. Returns the first error encountered with the
// offending index, so admin handlers can surface a precise 400.
func ParseSeverityList(in []string) ([]Severity, error) {
	out := make([]Severity, len(in))
	for i, s := range in {
		sev, err := Parse(s)
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		out[i] = sev
	}
	return out, nil
}

// State represents the lifecycle state of a fired alert.
type State string

const (
	StateFiring       State = "firing"
	StateAcknowledged State = "acknowledged"
	StateResolved     State = "resolved"
)

// AlertRule is the configuration template that governs when and how an alert fires.
type AlertRule struct {
	ID              string         `json:"id"`
	DisplayName     string         `json:"displayName"`
	SourceType      string         `json:"sourceType"`
	DefaultSeverity Severity       `json:"defaultSeverity"`
	RequiresAck     bool           `json:"requiresAck"`
	Enabled         bool           `json:"enabled"`
	Params          map[string]any `json:"params"`
	ParamsSchema    map[string]any `json:"paramsSchema"`
	CooldownSec     int            `json:"cooldownSec"`
	// GroupIDFilter is an optional per-group filter. NULL = fleet-wide;
	// non-NULL = rule only fires when the target device is a member of
	// this DeviceGroup. The Raiser applies the filter; the dispatcher
	// path is unchanged.
	GroupIDFilter *string   `json:"groupIdFilter,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Alert is a single fired alert instance.
type Alert struct {
	ID             string         `json:"id"`
	RuleID         string         `json:"ruleId"`
	SourceType     string         `json:"sourceType"`
	TargetKey      string         `json:"targetKey"`
	TargetLabel    string         `json:"targetLabel"`
	Severity       Severity       `json:"severity"`
	State          State          `json:"state"`
	Message        string         `json:"message"`
	Details        map[string]any `json:"details,omitempty"`
	FiredAt        time.Time      `json:"firedAt"`
	LastSeenAt     time.Time      `json:"lastSeenAt"`
	DuplicateCount int            `json:"duplicateCount"`
	AcknowledgedBy *string        `json:"acknowledgedBy,omitempty"`
	AcknowledgedAt *time.Time     `json:"acknowledgedAt,omitempty"`
	ResolvedAt     *time.Time     `json:"resolvedAt,omitempty"`
	ResolvedBy     *string        `json:"resolvedBy,omitempty"`
	ResolvedReason *string        `json:"resolvedReason,omitempty"`
}

// Channel is a notification delivery target (webhook, slack, email, etc.).
//
// Severities is the per-channel severity allow-list — an empty slice means
// "match all". Each entry is a typed Severity (lowercase canonical form);
// inbound admin payloads are validated via Severity.UnmarshalJSON, and
// rows read from the legacy text[] DB column are normalised via
// ParseLoose in store.scanChannelRows.
type Channel struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Type        string         `json:"type"`
	Enabled     bool           `json:"enabled"`
	Severities  []Severity     `json:"severities"`
	SourceTypes []string       `json:"sourceTypes"`
	Config      map[string]any `json:"config"`
	CreatedAt   time.Time      `json:"createdAt"`
	UpdatedAt   time.Time      `json:"updatedAt"`
}

// Dispatch records a single delivery attempt for an alert to a channel.
type Dispatch struct {
	ID          string    `json:"id"`
	AlertID     string    `json:"alertId"`
	ChannelID   string    `json:"channelId"`
	ChannelName string    `json:"channelName"`
	Success     bool      `json:"success"`
	StatusCode  *int      `json:"statusCode,omitempty"`
	ErrorMsg    *string   `json:"errorMsg,omitempty"`
	AttemptedAt time.Time `json:"attemptedAt"`
}

// ListFilter controls pagination and filtering for ListAlerts.
//
// The four categorical filters (State, Severity, SourceType, RuleID) are
// multi-value: an empty slice means "do not filter on this dimension", and a
// populated slice is OR'd inside the dimension (e.g. State=[firing,acknowledged]
// matches rows in either state). Dimensions are AND'd together.
type ListFilter struct {
	State      []string   `json:"state,omitempty"`
	Severity   []string   `json:"severity,omitempty"`
	SourceType []string   `json:"sourceType,omitempty"`
	RuleID     []string   `json:"ruleId,omitempty"`
	Since      *time.Time `json:"since,omitempty"`
	Until      *time.Time `json:"until,omitempty"`
	Offset     int        `json:"offset,omitempty"`
	Limit      int        `json:"limit,omitempty"`
}
