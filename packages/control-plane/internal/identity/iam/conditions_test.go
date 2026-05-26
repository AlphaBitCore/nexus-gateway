package iam

import "testing"

func TestEvaluateConditions(t *testing.T) {
	tests := []struct {
		name  string
		block ConditionBlock
		ctx   ConditionContext
		want  bool
	}{
		{"nil block", nil, ConditionContext{}, true},
		{"empty block", ConditionBlock{}, ConditionContext{}, true},
		{"StringEquals match", ConditionBlock{"StringEquals": {"env": "prod"}}, ConditionContext{"env": "prod"}, true},
		{"StringEquals mismatch", ConditionBlock{"StringEquals": {"env": "prod"}}, ConditionContext{"env": "dev"}, false},
		{"StringNotEquals match", ConditionBlock{"StringNotEquals": {"env": "prod"}}, ConditionContext{"env": "dev"}, true},
		{"StringLike wildcard", ConditionBlock{"StringLike": {"path": "admin:*"}}, ConditionContext{"path": "admin:ReadProvider"}, true},
		{"StringLike mismatch", ConditionBlock{"StringLike": {"path": "admin:*"}}, ConditionContext{"path": "proxy:Read"}, false},
		{"StringLike empty actual fails", ConditionBlock{"StringLike": {"path": "admin:*"}}, ConditionContext{}, false},
		{"IpAddress CIDR match", ConditionBlock{"IpAddress": {"nexus:SourceIp": "10.0.0.0/8"}}, ConditionContext{"nexus:SourceIp": "10.1.2.3"}, true},
		{"IpAddress CIDR mismatch", ConditionBlock{"IpAddress": {"nexus:SourceIp": "10.0.0.0/8"}}, ConditionContext{"nexus:SourceIp": "192.168.1.1"}, false},
		{"IpAddress empty actual fails", ConditionBlock{"IpAddress": {"ip": "10.0.0.0/8"}}, ConditionContext{}, false},
		{"NotIpAddress empty actual passes", ConditionBlock{"NotIpAddress": {"ip": "10.0.0.0/8"}}, ConditionContext{}, true},
		{"NotIpAddress", ConditionBlock{"NotIpAddress": {"nexus:SourceIp": "10.0.0.0/8"}}, ConditionContext{"nexus:SourceIp": "192.168.1.1"}, true},
		{"NumericLessThan", ConditionBlock{"NumericLessThan": {"cost": "100"}}, ConditionContext{"cost": "50"}, true},
		{"NumericGreaterThan", ConditionBlock{"NumericGreaterThan": {"cost": "100"}}, ConditionContext{"cost": "50"}, false},
		{"NumericGreaterThan true", ConditionBlock{"NumericGreaterThan": {"cost": "100"}}, ConditionContext{"cost": "200"}, true},
		{"NumericEquals true", ConditionBlock{"NumericEquals": {"cost": "100"}}, ConditionContext{"cost": "100"}, true},
		{"NumericEquals false", ConditionBlock{"NumericEquals": {"cost": "100"}}, ConditionContext{"cost": "50"}, false},
		{"NumericEquals unparseable actual fails", ConditionBlock{"NumericEquals": {"cost": "100"}}, ConditionContext{"cost": "free"}, false},
		{"DateLessThan true", ConditionBlock{"DateLessThan": {"t": "2026-12-31T00:00:00Z"}}, ConditionContext{"t": "2026-01-01T00:00:00Z"}, true},
		{"DateLessThan false", ConditionBlock{"DateLessThan": {"t": "2026-01-01T00:00:00Z"}}, ConditionContext{"t": "2026-12-31T00:00:00Z"}, false},
		{"DateLessThan unparseable actual fails", ConditionBlock{"DateLessThan": {"t": "2026-12-31T00:00:00Z"}}, ConditionContext{"t": "tomorrow"}, false},
		{"DateGreaterThan true", ConditionBlock{"DateGreaterThan": {"t": "2026-01-01T00:00:00Z"}}, ConditionContext{"t": "2026-12-31T00:00:00Z"}, true},
		{"DateGreaterThan false", ConditionBlock{"DateGreaterThan": {"t": "2026-12-31T00:00:00Z"}}, ConditionContext{"t": "2026-01-01T00:00:00Z"}, false},
		{"DateGreaterThan unparseable expected fails", ConditionBlock{"DateGreaterThan": {"t": "yesterday"}}, ConditionContext{"t": "2026-12-31T00:00:00Z"}, false},
		{"Unknown operator fails closed", ConditionBlock{"UnknownOp": {"x": "y"}}, ConditionContext{"x": "y"}, false},
		{"AND logic - both match", ConditionBlock{
			"StringEquals": {"env": "prod"},
			"IpAddress":    {"ip": "10.0.0.0/8"},
		}, ConditionContext{"env": "prod", "ip": "10.1.2.3"}, true},
		{"AND logic - one fails", ConditionBlock{
			"StringEquals": {"env": "prod"},
			"IpAddress":    {"ip": "10.0.0.0/8"},
		}, ConditionContext{"env": "dev", "ip": "10.1.2.3"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateConditions(tt.block, tt.ctx)
			if got != tt.want {
				t.Errorf("EvaluateConditions() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestMatchCIDR covers all branches in matchCIDR: exact-IP equality,
// malformed CIDR rejection, and malformed-IP rejection. evaluateOperator
// only reaches matchCIDR after the empty-actual guard, but the function
// itself is callable directly for the edge-case branches.
func TestMatchCIDR(t *testing.T) {
	cases := []struct {
		name string
		ip   string
		cidr string
		want bool
	}{
		{"exact ip match (no slash)", "10.0.0.1", "10.0.0.1", true},
		{"exact ip mismatch (no slash)", "10.0.0.2", "10.0.0.1", false},
		{"malformed cidr returns false", "10.0.0.1", "10.0.0.0/notanumber", false},
		{"valid cidr but malformed ip returns false", "not-an-ip", "10.0.0.0/8", false},
		{"cidr contains ip", "10.1.2.3", "10.0.0.0/8", true},
		{"cidr does not contain ip", "192.168.1.1", "10.0.0.0/8", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchCIDR(tc.ip, tc.cidr); got != tc.want {
				t.Errorf("matchCIDR(%q, %q) = %v, want %v", tc.ip, tc.cidr, got, tc.want)
			}
		})
	}
}
