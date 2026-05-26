package breakglass

import (
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// shadowProbeClient is the subset of *thingclient.Client that ShadowProbe
// requires. The narrow interface makes the probe unit-testable without
// standing up a real Hub connection: any fake that supplies
// LastReportedAtTime and HeartbeatInterval satisfies it, while
// *thingclient.Client continues to satisfy it in production.
type shadowProbeClient interface {
	LastReportedAtTime() time.Time
	HeartbeatInterval() time.Duration
}

// ShadowProbe adapts *thingclient.Client to health.ShadowProbe. The
// "stale" threshold is 2× the HTTP-fallback heartbeat interval: one missed
// heartbeat is a transient stall (scheduler, network hiccup), two back-to-
// back misses say the shadow path is actually broken.
type ShadowProbe struct {
	client shadowProbeClient
}

// NewShadowProbe returns a ShadowProbe wrapping the given client.
// *thingclient.Client satisfies shadowProbeClient, so production callers
// compile unchanged.
func NewShadowProbe(c *thingclient.Client) *ShadowProbe {
	return &ShadowProbe{client: c}
}

// HasReported reports whether the client has ever reported to Hub.
func (p *ShadowProbe) HasReported() bool {
	return !p.client.LastReportedAtTime().IsZero()
}

// LastReportAge returns the time elapsed since the last successful report.
func (p *ShadowProbe) LastReportAge() time.Duration {
	last := p.client.LastReportedAtTime()
	if last.IsZero() {
		return 0
	}
	return time.Since(last)
}

// StaleAfter returns the duration after which the shadow path is considered stale.
func (p *ShadowProbe) StaleAfter() time.Duration {
	return 2 * p.client.HeartbeatInterval()
}
