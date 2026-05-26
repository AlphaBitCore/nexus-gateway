//go:build darwin

package main

import (
	"log/slog"
	"os/exec"
	"time"
)

// flushMDNSResponderIfDarwin runs `dscacheutil -flushcache` and
// `killall -HUP mDNSResponder` so getaddrinfo (browsers, curl, every
// app that uses the system resolver) re-reads its name service config
// after our NETransparentProxyProvider tears down. Without this, the
// user uninstalls / quits / restarts the agent and finds DNS hangs
// ~5 s on every new HTTPS connection while `dig` direct-to-UDP/53
// works — a macOS-side cache invalidation race that happens reliably
// whenever the NE filter state changes from configured to
// not-configured. See memory:
// feedback_macos_mdns_flush_after_ne_state_change.
//
// The daemon runs as root via LaunchDaemon, which is the only context
// at shutdown time with the privs to kill -HUP mDNSResponder; the
// menu-bar host runs as the user and cannot.
func flushMDNSResponderIfDarwin() {
	t0 := time.Now()
	if out, err := exec.Command("/usr/bin/dscacheutil", "-flushcache").CombinedOutput(); err != nil {
		slog.Warn("flushMDNSResponder: dscacheutil -flushcache failed",
			"err", err,
			"out", string(out),
		)
	}
	if out, err := exec.Command("/usr/bin/killall", "-HUP", "mDNSResponder").CombinedOutput(); err != nil {
		slog.Warn("flushMDNSResponder: killall -HUP mDNSResponder failed",
			"err", err,
			"out", string(out),
		)
	}
	slog.Info("flushMDNSResponder: dscacheutil + killall -HUP mDNSResponder done",
		"duration_ms", time.Since(t0).Milliseconds(),
	)
}
