package traffic

import (
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/interception"
)

// Instance holds runtime state for one InterceptionDomain.
type Instance struct {
	Domain  InterceptionDomainConfig
	Adapter Adapter
	Paths   []InterceptionPathConfig // sorted by priority desc → specificity → createdAt
}

// InterceptionDomainConfig is the parsed, ready-to-match form of a DB row.
type InterceptionDomainConfig struct {
	ID                string
	Name              string
	HostPattern       string
	HostMatchType     interception.HostMatchType
	AdapterID         string
	Enabled           bool
	Priority          int32
	DefaultPathAction interception.DefaultPathAction
	OnAdapterError    interception.FailureAction
	NetworkZone       interception.NetworkZone
	Source            string
	CreatedAt         time.Time
}

// InterceptionPathConfig is the parsed, ready-to-match form of a path rule.
type InterceptionPathConfig struct {
	ID          string
	PathPattern []string
	MatchType   interception.PathMatchType
	Action      interception.PathAction
	Priority    int32
	Enabled     bool
	CreatedAt   time.Time
}

// DomainSnapshot is the atomic unit of hot-reload for ALL InterceptionDomain instances.
// All domains are bundled into a single snapshot and swapped atomically via one
// atomic.Pointer[DomainSnapshot]. This guarantees any single request always sees
// a fully consistent view of all domain rules.
type DomainSnapshot struct {
	Instances []*Instance          // sorted by priority desc for host matching
	ByHost    map[string]*Instance // fast path: exact-match host lookup
}

// BuildDomainSnapshot constructs a snapshot from DB config rows.
// domains and paths should be the full set from the database.
// Disabled domains/paths are filtered out. Unknown adapterIds are logged and skipped.
func BuildDomainSnapshot(
	domains []interception.InterceptionDomain,
	paths []interception.InterceptionPath,
	registry *AdapterRegistry,
	logger *slog.Logger,
) *DomainSnapshot {
	// Index paths by domainId.
	pathsByDomain := make(map[string][]interception.InterceptionPath)
	for _, p := range paths {
		if p.Enabled {
			pathsByDomain[p.DomainId] = append(pathsByDomain[p.DomainId], p)
		}
	}

	snap := &DomainSnapshot{
		ByHost: make(map[string]*Instance),
	}

	for _, d := range domains {
		if !d.Enabled {
			continue
		}

		factory := registry.Get(d.AdapterId)
		if factory == nil {
			logger.Warn("unknown adapterId, skipping domain",
				slog.String("domain", d.Name),
				slog.String("adapterId", d.AdapterId),
			)
			continue
		}

		adapter := factory()
		var adapterConfig map[string]any
		if d.AdapterConfig != nil {
			if err := json.Unmarshal(d.AdapterConfig, &adapterConfig); err != nil {
				logger.Warn("failed to parse adapterConfig, skipping domain",
					slog.String("domain", d.Name),
					slog.String("error", err.Error()),
				)
				continue
			}
		}
		if err := adapter.Configure(adapterConfig); err != nil {
			logger.Warn("adapter.Configure failed, skipping domain",
				slog.String("domain", d.Name),
				slog.String("error", err.Error()),
			)
			continue
		}

		inst := &Instance{
			Domain: InterceptionDomainConfig{
				ID:                d.Id,
				Name:              d.Name,
				HostPattern:       d.HostPattern,
				HostMatchType:     d.HostMatchType,
				AdapterID:         d.AdapterId,
				Enabled:           d.Enabled,
				Priority:          d.Priority,
				DefaultPathAction: d.DefaultPathAction,
				OnAdapterError:    d.OnAdapterError,
				NetworkZone:       d.NetworkZone,
				Source:            d.Source,
				CreatedAt:         d.CreatedAt,
			},
			Adapter: adapter,
		}

		// Build path configs.
		for _, p := range pathsByDomain[d.Id] {
			inst.Paths = append(inst.Paths, InterceptionPathConfig{
				ID:          p.Id,
				PathPattern: p.PathPattern,
				MatchType:   p.MatchType,
				Action:      p.Action,
				Priority:    p.Priority,
				Enabled:     p.Enabled,
				CreatedAt:   p.CreatedAt,
			})
		}
		sortPaths(inst.Paths)

		snap.Instances = append(snap.Instances, inst)
	}

	// Sort instances by priority desc, then createdAt asc. STABLE, matching
	// policy/domain's engine: two rules with equal priority AND equal createdAt
	// would otherwise get a nondeterministic order, and buildHostIndex bakes
	// that order into ByHost — turning a coin flip at snapshot-build time into a
	// fixed answer for the life of the snapshot. The shipped seed has 63 rows
	// all at priority 0 with distinct timestamps, so this is latent, not live.
	sort.SliceStable(snap.Instances, func(i, j int) bool {
		a, b := snap.Instances[i].Domain, snap.Instances[j].Domain
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})

	buildHostIndex(snap)

	if logger != nil {
		logger.Info("domain snapshot built",
			slog.Int("domains", len(snap.Instances)),
			slog.Int("exactHosts", len(snap.ByHost)),
		)
	}

	return snap
}

// sortPaths sorts path configs by: priority desc → specificity → createdAt asc.
func sortPaths(paths []InterceptionPathConfig) {
	sort.Slice(paths, func(i, j int) bool {
		a, b := paths[i], paths[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		specA, specB := matchTypeSpecificity(a.MatchType), matchTypeSpecificity(b.MatchType)
		if specA != specB {
			return specA > specB
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
}

// matchTypeSpecificity returns a rank for tiebreaking: EXACT > PREFIX > GLOB > REGEX.
func matchTypeSpecificity(mt interception.PathMatchType) int {
	switch mt {
	case interception.PathMatchTypeExact:
		return 4
	case interception.PathMatchTypePrefix:
		return 3
	case interception.PathMatchTypeGlob:
		return 2
	case interception.PathMatchTypeRegex:
		return 1
	default:
		return 0
	}
}

// Empty returns an empty snapshot (no domains configured).
func Empty() *DomainSnapshot {
	return &DomainSnapshot{
		ByHost: make(map[string]*Instance),
	}
}

// buildHostIndex fills ByHost with a MEMO OF THE SCAN rather than with the
// exact rules themselves. It runs after Instances is sorted, because what it
// memoises is the priority-ordered walk's answer.
//
// Two defects made the previous index a second, disagreeing definition of
// "which domain owns this host":
//
//   - It was consulted BEFORE the priority-ordered scan, so an exact rule always
//     beat a glob no matter what priority an admin set. Raising a broad rule
//     above a narrow one — the override lever the priority field exists to be —
//     did nothing.
//   - It was keyed on the stored pattern's own capitalisation while the scan
//     folds case, so the same policy resolved to different rules depending on
//     whether the request's casing happened to match the config's.
//
// Keys are folded, and each value is whatever scanForHost returns for that host,
// which may be a different and higher-priority instance than the exact rule the
// key came from. Building at snapshot time rather than memoising per request is
// deliberate: the Host header is caller-controlled, so a request-time cache is
// an unbounded map an attacker fills.
func buildHostIndex(s *DomainSnapshot) {
	for _, inst := range s.Instances {
		if inst.Domain.HostMatchType != interception.HostMatchTypeExact {
			continue
		}
		// ASCII only, and the lookup applies the same restriction. The key is
		// built with strings.ToLower while every matcher uses simple case
		// folding, and outside ASCII those are DIFFERENT equivalence relations:
		// ToLower("İ") is "i", so a memo keyed from one host can be hit by a
		// different one and answer with the wrong rule. Real hostnames are ASCII
		// (IDN arrives as punycode), so the fast path is not narrowed in
		// practice — and where the two relations could disagree, the scan
		// answers instead of a memo that cannot be trusted.
		if !isASCII(inst.Domain.HostPattern) {
			continue
		}
		host := strings.ToLower(inst.Domain.HostPattern)
		if _, seen := s.ByHost[host]; seen {
			continue
		}
		// Defensive: an exact pattern always matches its own lowercased form
		// via EqualFold, so the scan cannot come back empty here. Guarded
		// anyway rather than storing a nil into the fast path.
		if winner := scanForHost(s, host); winner != nil {
			s.ByHost[host] = winner
		}
	}
}

// isASCII reports whether s contains only single-byte runes, which is where
// strings.ToLower and simple case folding agree.
func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// scanForHost is the one definition of host resolution: the priority-ordered
// walk. ByHost caches its answers; nothing reimplements it.
func scanForHost(s *DomainSnapshot, host string) *Instance {
	for _, inst := range s.Instances {
		if matchHost(host, inst.Domain.HostPattern, inst.Domain.HostMatchType) {
			return inst
		}
	}
	return nil
}

// FindInstance looks up the best-matching Instance for a hostname.
// Returns nil if no domain matches.
func (s *DomainSnapshot) FindInstance(host string) *Instance {
	// Fast path: a memoised answer for a host some exact rule names. The key is
	// folded because hostnames are case-insensitive, and consulted only for an
	// ASCII host — see buildHostIndex for why the two folding relations must not
	// be mixed.
	if isASCII(host) {
		if inst, ok := s.ByHost[strings.ToLower(host)]; ok {
			return inst
		}
	}
	return scanForHost(s, host)
}

// ResolveAction determines the FilterResult for a request to host+path.
// Returns the matching instance (if any), the effective action, and the
// matched path rule (nil if default action applies).
func (s *DomainSnapshot) ResolveAction(host, path string) (*Instance, FilterResult, *InterceptionPathConfig) {
	inst := s.FindInstance(host)
	if inst == nil {
		return nil, Passthrough, nil // unknown domain → passthrough
	}

	// Check path rules.
	for i := range inst.Paths {
		p := &inst.Paths[i]
		if !p.Enabled {
			continue
		}
		if matchPathRule(path, p) {
			return inst, pathActionToFilterResult(p.Action), p
		}
	}

	// No path rule matched — apply domain default.
	return inst, defaultPathActionToFilterResult(inst.Domain.DefaultPathAction), nil
}

func pathActionToFilterResult(a interception.PathAction) FilterResult {
	switch a {
	case interception.PathActionProcess:
		return Process
	case interception.PathActionPassthrough:
		return Passthrough
	case interception.PathActionBlock:
		return Block
	default:
		return Passthrough
	}
}

func defaultPathActionToFilterResult(a interception.DefaultPathAction) FilterResult {
	switch a {
	case interception.DefaultPathActionProcess:
		return Process
	case interception.DefaultPathActionPassthrough:
		return Passthrough
	case interception.DefaultPathActionBlock:
		return Block
	default:
		return Passthrough
	}
}

// Size returns the total number of enabled domains in the snapshot.
func (s *DomainSnapshot) Size() int {
	return len(s.Instances)
}

// Domains returns a summary of domain names for logging.
func (s *DomainSnapshot) Domains() []string {
	names := make([]string, len(s.Instances))
	for i, inst := range s.Instances {
		names[i] = fmt.Sprintf("%s(%s)", inst.Domain.Name, inst.Domain.HostPattern)
	}
	return names
}

// HostPatterns returns the raw host pattern strings (e.g. "chatgpt.com",
// "*.openai.com") of every enabled instance in this snapshot. Consumers
// such as policy.Engine use this to test "is the destination host one
// the admin wants intercepted?" without copying the whole instance
// list. Disabled instances are skipped — they're configured but not
// active.
func (s *DomainSnapshot) HostPatterns() []string {
	out := make([]string, 0, len(s.Instances))
	for _, inst := range s.Instances {
		if !inst.Domain.Enabled {
			continue
		}
		if inst.Domain.HostPattern != "" {
			out = append(out, inst.Domain.HostPattern)
		}
	}
	return out
}
