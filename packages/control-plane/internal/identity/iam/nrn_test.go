package iam

import "testing"

func TestParseNRN(t *testing.T) {
	tests := []struct {
		input string
		want  *NRNComponents
	}{
		{"nrn:nexus:gateway:*:provider/openai", &NRNComponents{"gateway", "*", "provider", "openai"}},
		{"nrn:nexus:iam:org-acme/engineering:api-key/*", &NRNComponents{"iam", "org-acme/engineering", "api-key", "*"}},
		{"invalid", nil},
		{"nrn:nexus:gateway", nil},           // missing parts
		{"nrn:nexus:gateway:*:noslash", nil}, // missing /
	}
	for _, tt := range tests {
		got := ParseNRN(tt.input)
		if tt.want == nil {
			if got != nil {
				t.Errorf("ParseNRN(%q) = %+v, want nil", tt.input, got)
			}
			continue
		}
		if got == nil {
			t.Errorf("ParseNRN(%q) = nil, want %+v", tt.input, tt.want)
			continue
		}
		if *got != *tt.want {
			t.Errorf("ParseNRN(%q) = %+v, want %+v", tt.input, got, tt.want)
		}
	}
}

func TestBuildNRN(t *testing.T) {
	got := BuildNRN("gateway", "*", "provider", "openai")
	want := "nrn:nexus:gateway:*:provider/openai"
	if got != want {
		t.Errorf("BuildNRN = %q, want %q", got, want)
	}
}

func TestMatchNRN(t *testing.T) {
	tests := []struct {
		pattern, target string
		want            bool
	}{
		// Exact match
		{"nrn:nexus:gateway:*:provider/openai", "nrn:nexus:gateway:*:provider/openai", true},
		// Wildcard resource ID
		{"nrn:nexus:gateway:*:provider/*", "nrn:nexus:gateway:*:provider/openai", true},
		// Wildcard all
		{"nrn:nexus:*:*:*/*", "nrn:nexus:gateway:org-acme:provider/openai", true},
		// Hierarchical scope
		{"nrn:nexus:gateway:org-acme:provider/*", "nrn:nexus:gateway:org-acme/engineering:provider/openai", true},
		// Scope mismatch
		{"nrn:nexus:gateway:org-acme:provider/*", "nrn:nexus:gateway:org-other:provider/openai", false},
		// Service mismatch
		{"nrn:nexus:admin:*:provider/*", "nrn:nexus:gateway:*:provider/openai", false},
		// Resource type mismatch
		{"nrn:nexus:gateway:*:model/*", "nrn:nexus:gateway:*:provider/openai", false},
		// Glob in resource ID
		{"nrn:nexus:gateway:*:provider/gpt-*", "nrn:nexus:gateway:*:provider/gpt-4o", true},
		{"nrn:nexus:gateway:*:provider/gpt-*", "nrn:nexus:gateway:*:provider/claude-3", false},
	}
	for _, tt := range tests {
		got := MatchNRN(tt.pattern, tt.target)
		if got != tt.want {
			t.Errorf("MatchNRN(%q, %q) = %v, want %v", tt.pattern, tt.target, got, tt.want)
		}
	}
}

func TestBuildRequestNRNForAction(t *testing.T) {
	tests := []struct {
		name   string
		action string
		want   string
	}{
		{
			// Canonical gateway action → service + resource derived from catalog.
			name:   "canonical gateway",
			action: "admin:provider.read",
			want:   "nrn:nexus:gateway:*:provider/*",
		},
		{
			// Carved-out resource — compliance service.
			name:   "canonical compliance carved",
			action: "admin:payload-capture.read",
			want:   "nrn:nexus:compliance:*:payload-capture/*",
		},
		{
			// Carved-out resource — agent service.
			name:   "canonical agent carved",
			action: "admin:device-defaults.update",
			want:   "nrn:nexus:agent:*:device-defaults/*",
		},
		{
			// Carved-out resource — gateway service.
			name:   "canonical gateway carved",
			action: "admin:prompt-cache.update",
			want:   "nrn:nexus:gateway:*:prompt-cache/*",
		},
		{
			// Platform service action.
			name:   "canonical platform",
			action: "admin:alert.acknowledge",
			want:   "nrn:nexus:platform:*:alert/*",
		},
		{
			// IAM service action.
			name:   "canonical iam",
			action: "admin:audit-log.read",
			want:   "nrn:nexus:iam:*:audit-log/*",
		},
		{
			// Role-identity marker for VK-authenticated gateway invocation —
			// not in catalog, falls back to fully wildcarded NRN so a
			// Resource: "*" policy still authorises it.
			name:   "non-canonical gateway invoke",
			action: "gateway:invoke:*",
			want:   "nrn:nexus:*:*:*/*",
		},
		{
			// Agent role-identity marker.
			name:   "non-canonical ai-guard invoke",
			action: "ai-guard:invoke",
			want:   "nrn:nexus:*:*:*/*",
		},
		{
			// Device heartbeat — non-canonical action.
			name:   "non-canonical device heartbeat",
			action: "device:heartbeat",
			want:   "nrn:nexus:*:*:*/*",
		},
		{
			// Canonical shape but resource not in catalog: ParseAction
			// returns ok=true but ServiceForAction returns ok=false, so
			// resourceType is preserved while service stays "*".
			name:   "canonical-shape unknown resource",
			action: "admin:phantom-resource.read",
			want:   "nrn:nexus:*:*:phantom-resource/*",
		},
		{
			// Empty action → both derivations bail out.
			name:   "empty action",
			action: "",
			want:   "nrn:nexus:*:*:*/*",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildRequestNRNForAction(tt.action)
			if got != tt.want {
				t.Errorf("BuildRequestNRNForAction(%q) = %q, want %q", tt.action, got, tt.want)
			}
			// Regression guard: the produced NRN must round-trip through
			// MatchNRN against the seed.ts canonical policy pattern.
			if tt.want != "nrn:nexus:*:*:*/*" {
				policyPattern := tt.want
				if !MatchNRN(policyPattern, tt.want) {
					t.Errorf("MatchNRN(%q, %q) = false; policy-pattern self-match must always succeed", policyPattern, tt.want)
				}
			}
		})
	}
}

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern, target string
		want            bool
	}{
		{"*", "anything", true},
		{"admin:*", "admin:ReadProvider", true},
		{"admin:Read*", "admin:ReadProvider", true},
		{"admin:Read*", "admin:WriteProvider", false},
		{"*Provider", "admin:ReadProvider", true},
		{"*Provider", "admin:ReadModel", false},
		{"exact", "exact", true},
		{"exact", "other", false},
	}
	for _, tt := range tests {
		got := globMatch(tt.pattern, tt.target)
		if got != tt.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pattern, tt.target, got, tt.want)
		}
	}
}

// TestBuildDeviceCandidateNRNs covers the candidate-NRN expansion the
// IAM middleware feeds into EvaluateMulti when a route operates on a
// specific device. The unscoped form must always come first (so
// fleet-wide policies short-circuit), and each group memberships
// expands into a `group:<id>/<deviceID>` candidate.
func TestBuildDeviceCandidateNRNs(t *testing.T) {
	tests := []struct {
		name      string
		action    string
		deviceID  string
		groups    []string
		wantCount int
		wantFirst string // unscoped form must always be the first entry
		mustHave  []string
	}{
		{
			name:      "no groups → unscoped only",
			action:    "admin:agent-device.rotate",
			deviceID:  "dev-1",
			groups:    nil,
			wantCount: 1,
			wantFirst: "nrn:nexus:agent:*:agent-device/dev-1",
		},
		{
			name:      "one group → unscoped + one scoped",
			action:    "admin:agent-device.rotate",
			deviceID:  "dev-1",
			groups:    []string{"sg"},
			wantCount: 2,
			wantFirst: "nrn:nexus:agent:*:agent-device/dev-1",
			mustHave:  []string{"nrn:nexus:agent:*:agent-device/group:sg/dev-1"},
		},
		{
			name:      "multi-group → unscoped + N scoped",
			action:    "admin:agent-device.rotate",
			deviceID:  "dev-1",
			groups:    []string{"sg", "fra"},
			wantCount: 3,
			wantFirst: "nrn:nexus:agent:*:agent-device/dev-1",
			mustHave: []string{
				"nrn:nexus:agent:*:agent-device/group:sg/dev-1",
				"nrn:nexus:agent:*:agent-device/group:fra/dev-1",
			},
		},
		{
			name:      "empty-string group skipped",
			action:    "admin:agent-device.read",
			deviceID:  "dev-2",
			groups:    []string{"", "sg"},
			wantCount: 2,
			wantFirst: "nrn:nexus:agent:*:agent-device/dev-2",
			mustHave:  []string{"nrn:nexus:agent:*:agent-device/group:sg/dev-2"},
		},
		{
			name:      "non-canonical action falls back to wildcard",
			action:    "ai-guard:invoke",
			deviceID:  "dev-3",
			groups:    []string{"sg"},
			wantCount: 2,
			wantFirst: "nrn:nexus:*:*:*/dev-3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildDeviceCandidateNRNs(tt.action, tt.deviceID, tt.groups)
			if len(got) != tt.wantCount {
				t.Fatalf("got %d candidates, want %d: %v", len(got), tt.wantCount, got)
			}
			if got[0] != tt.wantFirst {
				t.Errorf("first candidate = %q, want %q", got[0], tt.wantFirst)
			}
			for _, m := range tt.mustHave {
				found := false
				for _, g := range got {
					if g == m {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("missing required candidate %q in %v", m, got)
				}
			}
		})
	}
}

// TestMatchNRN_GroupScopedResource pins the wire-grammar contract used
// by EvaluateMulti to enforce group-scoped policies. Patterns of the
// form `agent-device/group:<id>/*` must match exactly the candidate
// NRNs produced by BuildDeviceCandidateNRNs for member devices, and
// must not match the unscoped form or other groups' candidates.
func TestMatchNRN_GroupScopedResource(t *testing.T) {
	pattern := "nrn:nexus:agent:*:agent-device/group:sg/*"

	tests := []struct {
		target string
		want   bool
	}{
		{"nrn:nexus:agent:*:agent-device/group:sg/dev-1", true},
		{"nrn:nexus:agent:*:agent-device/group:sg/another-dev", true},
		{"nrn:nexus:agent:*:agent-device/group:fra/dev-1", false},
		{"nrn:nexus:agent:*:agent-device/dev-1", false}, // unscoped form
		{"nrn:nexus:agent:*:agent-device/*", false},     // wildcard NOT a scope match
	}
	for _, tt := range tests {
		got := MatchNRN(pattern, tt.target)
		if got != tt.want {
			t.Errorf("MatchNRN(%q, %q) = %v, want %v", pattern, tt.target, got, tt.want)
		}
	}
}

// TestMatchNRN_UnscopedPatternStillMatchesGroupCandidate confirms the
// backward-compat axis: a fleet-wide policy with Resource
// `agent-device/*` must continue to allow actions even when the
// candidate list includes group-scoped forms (i.e. when a route uses
// the device-aware middleware variant).
func TestMatchNRN_UnscopedPatternStillMatchesGroupCandidate(t *testing.T) {
	pattern := "nrn:nexus:agent:*:agent-device/*"
	// The unscoped pattern's ResourceID segment is "*", which matches
	// anything via matchSegment — INCLUDING the group: prefix path.
	// This is intentional: an admin granting agent-device/* should
	// continue to manage every device regardless of group membership.
	got := MatchNRN(pattern, "nrn:nexus:agent:*:agent-device/group:sg/dev-1")
	if !got {
		t.Errorf("unscoped wildcard %q must match group-scoped candidate", pattern)
	}
	got = MatchNRN(pattern, "nrn:nexus:agent:*:agent-device/dev-1")
	if !got {
		t.Errorf("unscoped wildcard %q must match unscoped candidate", pattern)
	}
}
