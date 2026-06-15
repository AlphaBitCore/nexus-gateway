package status

import "time"

const (
	degradedQueueThreshold = 100
	degradedCertDays       = 30
)

func (c *Collector) computeState(unsyncedCount int) (string, string) {
	if !c.enrolled {
		return "error", "Device not enrolled"
	}
	if !c.gatewayConnected {
		return "degraded", "Gateway unreachable"
	}
	if reason, ok := c.checkInterceptionHealth(); !ok {
		return "degraded", reason
	}
	if unsyncedCount > degradedQueueThreshold {
		return "degraded", "Audit queue backlog"
	}
	if !c.certExpiresAt.IsZero() && time.Until(c.certExpiresAt) < degradedCertDays*24*time.Hour {
		return "degraded", "Certificate expiring soon"
	}
	return "active", ""
}

// checkInterceptionHealth returns (reason, false) when the OS capture
// layer is not attached and the post-startup grace period has elapsed.
// (_, true) means healthy (or "unknown — Health provider not wired"; in
// either case the caller continues with normal state computation).
//
// Two failure modes are distinguished so the menu-bar / Dashboard can
// surface an actionable hint:
//
//   - "Network filter not connected" — first attach has never happened
//     since daemon startup; almost always means the user did not
//     approve the macOS proxy-configuration dialog (or the equivalent
//     on linux/windows).
//   - "Network filter detached" — at least one attach happened, but no
//     session is currently active; usually a crashed extension or a
//     user toggling the filter off in System Settings.
//
// Platforms whose capture layer is always-on rather than
// connection-driven (Linux iptables: the redirect chain is live the
// moment the reconciler installs it, so zero flows on an idle host is
// healthy) set Health.SelfReported and supply their own
// Health.DegradedReason. For those the connection-count heuristic
// below is skipped entirely — applying it would mis-flag every idle
// Linux host as "Network filter not connected".
func (c *Collector) checkInterceptionHealth() (string, bool) {
	if c.interceptionHealthFn == nil {
		return "", true
	}
	h := c.interceptionHealthFn()
	now := time.Now()
	if c.nowFn != nil {
		now = c.nowFn()
	}
	if !h.StartedAt.IsZero() && now.Sub(h.StartedAt) < InterceptionGracePeriod {
		return "", true // still warming up
	}
	// Self-reporting platforms own their verdict: trust DegradedReason
	// (empty = healthy) and never fall through to the count heuristic.
	if h.SelfReported {
		if h.DegradedReason != "" {
			return h.DegradedReason, false
		}
		return "", true
	}
	if h.ConnectionsTotal == 0 {
		return "Network filter not connected", false
	}
	if h.ActiveSessions == 0 {
		return "Network filter detached", false
	}
	return "", true
}
