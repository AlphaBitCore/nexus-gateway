package identity

// ActiveExemption is a single entry in compliance-proxy's exemptions
// template. Authored by CP (on approval of an ExemptionRequest) and applied
// by proxy/internal/exemption/store.go.
type ActiveExemption struct {
	ID         string `json:"id"`
	SourceIP   string `json:"sourceIP"`
	TargetHost string `json:"targetHost"`
	// ExpiresAt is RFC3339. Proxy filters locally; CP's GC job prunes expired
	// entries and bumps the template version.
	ExpiresAt string `json:"expiresAt"`
	// EffectiveFrom is RFC3339 when the exemption may begin matching traffic.
	// Empty means "effective immediately" for backward compatibility.
	EffectiveFrom string `json:"effectiveFrom,omitempty"`
	Reason        string `json:"reason"`
	ApprovedBy    string `json:"approvedBy"`
	// Disabled keeps the row in the shadow template but the proxy must not
	// match it (soft off). Omitted or false means the exemption is effective.
	Disabled bool `json:"disabled,omitempty"`
}

// ActiveExemptions is the full shadow payload for config_key="exemptions".
type ActiveExemptions struct {
	Entries []ActiveExemption `json:"entries"`
}
