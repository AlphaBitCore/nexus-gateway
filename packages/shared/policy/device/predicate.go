// Package device evaluates a smart-group membership predicate against a
// single device's attributes.
//
// Wire shape:
//
//	{
//	  "all": [
//	    {"field": "os", "op": "in", "value": ["darwin", "linux"]},
//	    {"field": "agentVersion", "op": "ge", "value": "1.5.0"},
//	    {"field": "primaryIp", "op": "cidr", "value": "10.32.0.0/16"},
//	    {"field": "boundUserOrgPath", "op": "prefix", "value": "corp/finance/"}
//	  ]
//	}
//
// Top-level wrapper is `all`/`any`. Each leaf is a {field, op, value}
// triplet. The field set and op set are closed — no user-supplied SQL,
// no script execution, no recursive nesting beyond all/any composition.
//
// Used by Hub (per-heartbeat + 60s membership recompute) and by the
// CP admin API dry-run preview endpoint.
package device

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

// Device is the attribute snapshot a predicate evaluates against.
// Fields mirror the closed attribute set in the SDD. Time fields are
// expressed as `seconds-since-epoch` (0 = unknown) so the
// `relative_seconds_within` op can compare against a `now` reference
// the caller supplies via Evaluate's `now` parameter.
type Device struct {
	OS               string
	OSVersion        string
	AgentVersion     string
	Hostname         string
	PrimaryIP        string
	PhysicalID       string
	Status           string
	BoundUserID      string
	BoundUserOrgPath string
	// Unix-seconds; 0 = unknown / not-applicable.
	EnrolledAtSec    int64
	LastHeartbeatSec int64
	// Metadata is an escape hatch for ad-hoc string labels keyed by
	// `metadata.<key>` in the predicate. Values must be strings; the
	// matcher does not coerce other types.
	Metadata map[string]string
	// IdpGroupIDs is the set of IamGroup ids the device's bound user
	// is a member of (`IamGroupMembership.principalId = boundUserId`).
	// Pre-loaded by the recompute query so the matcher stays pure /
	// stateless. Empty when no user is bound or the user has no
	// IdP-synced group memberships. The `idp_group_member` predicate
	// operator checks membership against this slice.
	IdpGroupIDs []string
	// Tags are free-form string labels. The `tags_contains` operator
	// checks membership against this slice.
	Tags []string
}

// Predicate is the parsed JSON wire shape. One of All or Any must be
// non-nil at the top level; nested groups are not allowed.
type Predicate struct {
	All []Leaf `json:"all,omitempty"`
	Any []Leaf `json:"any,omitempty"`
}

// Leaf is one {field, op, value} triplet.
type Leaf struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value"`
}

// Evaluate returns whether the device satisfies the predicate.
// `now` is used by relative-time ops; pass 0 when no time comparison
// is expected (other ops ignore it).
//
// Returns an explanatory error only for predicate-shape mistakes
// (unknown field / op, malformed regex). A field that's empty on the
// device — e.g. predicate references `primaryIp` but the device
// hasn't reported one — is NOT an error; it just doesn't match
// (treat as missing). The caller decides how to escalate.
func Evaluate(p Predicate, d *Device, nowSec int64) (bool, error) {
	if len(p.All) > 0 && len(p.Any) > 0 {
		return false, fmt.Errorf("predicate: top level must be exactly one of `all` or `any`")
	}
	if len(p.All) == 0 && len(p.Any) == 0 {
		// Empty predicate matches nothing — explicit empty-list is a
		// quarantine pattern (the operator wants zero members today).
		// Compare against the dry-run preview output of 0.
		return false, nil
	}
	if len(p.All) > 0 {
		for _, leaf := range p.All {
			ok, err := evalLeaf(leaf, d, nowSec)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}
		}
		return true, nil
	}
	for _, leaf := range p.Any {
		ok, err := evalLeaf(leaf, d, nowSec)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func evalLeaf(leaf Leaf, d *Device, nowSec int64) (bool, error) {
	got, ok := fieldValue(leaf.Field, d)
	if !ok {
		return false, fmt.Errorf("predicate: unknown field %q", leaf.Field)
	}
	switch leaf.Op {
	case "eq":
		return eq(got, leaf.Value), nil
	case "ne":
		return !eq(got, leaf.Value), nil
	case "in":
		return inList(got, leaf.Value), nil
	case "nin":
		return !inList(got, leaf.Value), nil
	case "prefix":
		s, _ := got.(string)
		v, _ := leaf.Value.(string)
		return strings.HasPrefix(s, v), nil
	case "regex":
		s, _ := got.(string)
		v, _ := leaf.Value.(string)
		if v == "" {
			return false, fmt.Errorf("predicate: empty regex for field %q", leaf.Field)
		}
		re, err := regexp.Compile(v)
		if err != nil {
			return false, fmt.Errorf("predicate: bad regex %q for field %q: %w", v, leaf.Field, err)
		}
		return re.MatchString(s), nil
	case "cidr":
		s, _ := got.(string)
		v, _ := leaf.Value.(string)
		if s == "" {
			return false, nil
		}
		_, ipnet, err := net.ParseCIDR(v)
		if err != nil {
			return false, fmt.Errorf("predicate: bad cidr %q: %w", v, err)
		}
		ip := net.ParseIP(s)
		if ip == nil {
			return false, nil
		}
		return ipnet.Contains(ip), nil
	case "lt", "le", "gt", "ge":
		return cmpInt(got, leaf.Value, leaf.Op)
	case "tags_contains":
		// Operates on the device's Tags slice. value is either a string
		// (single tag) or string list (any-of). `tags_contains_all`
		// is the strict-AND variant.
		if leaf.Field != "tags" {
			return false, fmt.Errorf("predicate: tags_contains only valid on field=tags")
		}
		if want, ok := leaf.Value.(string); ok {
			for _, t := range d.Tags {
				if t == want {
					return true, nil
				}
			}
			return false, nil
		}
		if list, ok := leaf.Value.([]any); ok {
			for _, v := range list {
				vs, _ := v.(string)
				for _, t := range d.Tags {
					if t == vs {
						return true, nil
					}
				}
			}
			return false, nil
		}
		return false, fmt.Errorf("predicate: tags_contains needs string or string-list value")
	case "tags_contains_all":
		if leaf.Field != "tags" {
			return false, fmt.Errorf("predicate: tags_contains_all only valid on field=tags")
		}
		list, ok := leaf.Value.([]any)
		if !ok {
			return false, fmt.Errorf("predicate: tags_contains_all needs string-list value")
		}
		for _, v := range list {
			vs, _ := v.(string)
			found := false
			for _, t := range d.Tags {
				if t == vs {
					found = true
					break
				}
			}
			if !found {
				return false, nil
			}
		}
		return true, nil
	case "idp_group_member":
		// Special-case: operates on the device's IdpGroupIDs slice
		// rather than the resolved field value. `field` must be the
		// sentinel "idpGroup" (the JSON wire shape uses
		// {"field":"idpGroup","op":"idp_group_member","value":"<group-id>"}).
		// `value` is the IamGroup id; matches when the device's bound
		// user is in that group (or any of the listed groups when
		// `value` is a string slice).
		if leaf.Field != "idpGroup" {
			return false, fmt.Errorf("predicate: idp_group_member only valid on field=idpGroup")
		}
		want, ok := leaf.Value.(string)
		if ok {
			for _, g := range d.IdpGroupIDs {
				if g == want {
					return true, nil
				}
			}
			return false, nil
		}
		// Allow a list — `value: ["g1","g2"]` means "in any of these".
		if list, ok := leaf.Value.([]any); ok {
			for _, v := range list {
				vs, _ := v.(string)
				for _, g := range d.IdpGroupIDs {
					if g == vs {
						return true, nil
					}
				}
			}
			return false, nil
		}
		return false, fmt.Errorf("predicate: idp_group_member needs string or string-list value")
	case "relative_seconds_within":
		got64, ok := got.(int64)
		if !ok || got64 == 0 {
			return false, nil
		}
		v64, ok := toInt64(leaf.Value)
		if !ok {
			return false, fmt.Errorf("predicate: relative_seconds_within needs numeric value")
		}
		if nowSec <= 0 {
			return false, fmt.Errorf("predicate: relative_seconds_within requires nowSec > 0")
		}
		// "within v seconds of now" — Δ ≤ v
		delta := nowSec - got64
		if delta < 0 {
			delta = -delta
		}
		return delta <= v64, nil
	default:
		return false, fmt.Errorf("predicate: unknown op %q", leaf.Op)
	}
}

// fieldValue resolves a closed-set field name to its typed value on
// the device. Returns (value, false) when the field name is unknown
// (callers surface this as a predicate-shape error). Metadata access
// uses the `metadata.<key>` dotted form.
func fieldValue(name string, d *Device) (any, bool) {
	if strings.HasPrefix(name, "metadata.") {
		k := name[len("metadata."):]
		v, ok := d.Metadata[k]
		if !ok {
			return "", true // present-but-empty — does not match, but not a shape error
		}
		return v, true
	}
	switch name {
	case "os":
		return d.OS, true
	case "osVersion":
		return d.OSVersion, true
	case "agentVersion":
		return d.AgentVersion, true
	case "hostname":
		return d.Hostname, true
	case "primaryIp":
		return d.PrimaryIP, true
	case "physicalId":
		return d.PhysicalID, true
	case "status":
		return d.Status, true
	case "boundUserId":
		return d.BoundUserID, true
	case "boundUserOrgPath":
		return d.BoundUserOrgPath, true
	case "enrolledAt":
		return d.EnrolledAtSec, true
	case "lastHeartbeat":
		return d.LastHeartbeatSec, true
	case "idpGroup":
		// Sentinel: idp_group_member resolves against IdpGroupIDs
		// directly. We return an empty string + ok=true so fieldValue's
		// caller doesn't error before reaching the operator switch.
		return "", true
	case "tags":
		// Same sentinel pattern as idpGroup — tags_contains /
		// tags_contains_all consult the device's Tags slice
		// directly and don't need a resolved field value.
		return "", true
	}
	return nil, false
}

func eq(got any, want any) bool {
	gs, gok := got.(string)
	ws, wok := want.(string)
	if gok && wok {
		return gs == ws
	}
	gi, gok := toInt64(got)
	wi, wok := toInt64(want)
	if gok && wok {
		return gi == wi
	}
	return false
}

func inList(got any, value any) bool {
	list, ok := value.([]any)
	if !ok {
		return false
	}
	for _, v := range list {
		if eq(got, v) {
			return true
		}
	}
	return false
}

func cmpInt(got any, want any, op string) (bool, error) {
	// Semver comparison support: when both sides are dotted-decimal
	// version strings, compare component-wise. Otherwise integer.
	gs, gok := got.(string)
	ws, wok := want.(string)
	if gok && wok && looksLikeVersion(gs) && looksLikeVersion(ws) {
		cmp := compareVersion(gs, ws)
		switch op {
		case "lt":
			return cmp < 0, nil
		case "le":
			return cmp <= 0, nil
		case "gt":
			return cmp > 0, nil
		case "ge":
			return cmp >= 0, nil
		}
	}
	gi, gok := toInt64(got)
	wi, wok := toInt64(want)
	if !gok || !wok {
		return false, fmt.Errorf("predicate: %s needs comparable values", op)
	}
	switch op {
	case "lt":
		return gi < wi, nil
	case "le":
		return gi <= wi, nil
	case "gt":
		return gi > wi, nil
	case "ge":
		return gi >= wi, nil
	}
	return false, fmt.Errorf("predicate: unknown cmp op %q", op)
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	}
	return 0, false
}

func looksLikeVersion(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return strings.Contains(s, ".")
}

// compareVersion does a component-wise numeric comparison of dotted
// version strings. Missing components are treated as 0 ("1.5" < "1.5.1").
func compareVersion(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := range n {
		ai := int64(0)
		bi := int64(0)
		if i < len(as) {
			_, _ = fmt.Sscanf(as[i], "%d", &ai)
		}
		if i < len(bs) {
			_, _ = fmt.Sscanf(bs[i], "%d", &bi)
		}
		if ai < bi {
			return -1
		}
		if ai > bi {
			return 1
		}
	}
	return 0
}
