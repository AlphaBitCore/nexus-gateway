package local

import (
	"net/http"
	"time"

	"golang.org/x/net/http2"
)

// EnableH2Health turns on HTTP/2 PING-frame health checking on tr.
//
// Why this matters for the CLI: prod is served over HTTP/2, where ALL requests to a
// host multiplex onto a SINGLE long-lived connection. When a NAT/firewall on the
// user's network silently drops that idle connection (no FIN), Go's h1-oriented
// IdleConnTimeout does NOT close the h2 connection, so the client keeps writing every
// request onto the dead connection and each one hangs to the client/turn timeout —
// observed as minutes of "context deadline exceeded" with reused=true and no recovery.
//
// ReadIdleTimeout makes the h2 transport send a PING frame once the connection has been
// idle that long; if no PONG arrives within PingTimeout the connection is considered
// dead, closed, and the next request opens a fresh one. The PING also keeps the NAT
// mapping warm so a healthy connection is not dropped in the first place. This mirrors
// shared/transport/http and the agent's H2ReadIdleTimeout — the CLI's transport was the
// only one in the tree missing it. ForceAttemptHTTP2 alone does not expose these knobs;
// an explicit ConfigureTransports is required.
func EnableH2Health(tr *http.Transport) {
	configureH2Health(tr, 15*time.Second, 5*time.Second)
}

// configureH2Health is the parameterised core (the test drives it with short timeouts
// so the recovery path can be exercised deterministically without idling 15s).
func configureH2Health(tr *http.Transport, readIdle, ping time.Duration) {
	if h2tr, err := http2.ConfigureTransports(tr); err == nil && h2tr != nil {
		h2tr.ReadIdleTimeout = readIdle
		h2tr.PingTimeout = ping
	}
}
