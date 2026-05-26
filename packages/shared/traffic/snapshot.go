package traffic

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
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

		// Fast-path index for exact-match hosts.
		if d.HostMatchType == interception.HostMatchTypeExact {
			snap.ByHost[d.HostPattern] = inst
		}
	}

	// Sort instances by priority desc, then createdAt asc.
	sort.Slice(snap.Instances, func(i, j int) bool {
		a, b := snap.Instances[i].Domain, snap.Instances[j].Domain
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})

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

// FindInstance looks up the best-matching Instance for a hostname.
// Returns nil if no domain matches.
func (s *DomainSnapshot) FindInstance(host string) *Instance {
	// Fast path: exact match.
	if inst, ok := s.ByHost[host]; ok {
		return inst
	}
	// Slow path: iterate sorted instances and try matchers.
	for _, inst := range s.Instances {
		if matchHost(host, inst.Domain.HostPattern, inst.Domain.HostMatchType) {
			return inst
		}
	}
	return nil
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
