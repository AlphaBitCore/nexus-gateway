package interception

// Killswitch is the shadow payload for config_key="killswitch" on both
// compliance-proxy and agent. Mirrors the shape the runtimeapi endpoints
// and CP admin handlers read/write.
//
// Wire semantic (canonical, shared by both receivers): Engaged=true means
// the kill switch is engaged — the data-plane MUST passthrough without
// TLS-bumping. Engaged=false (the fail-safe default) means normal
// operation. CP UI's "Engage" button writes Engaged=true and both
// receivers act identically on the wire value; no inversion anywhere.
type Killswitch struct {
	Engaged bool `json:"engaged"`
}
