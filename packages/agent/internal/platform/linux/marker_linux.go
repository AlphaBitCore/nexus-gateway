//go:build linux

package linux

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// AgentSOMark is the SO_MARK value the agent sets on every outbound TCP
// socket it creates. The Linux iptables NEXUS_AGENT chain has a
// matching `-m mark --mark 0x4E58 -j RETURN` rule at the top, which
// short-circuits the agent's own egress out of the REDIRECT path —
// preventing the self-loop that would otherwise occur when the
// proxy's upstream dial gets caught by its own redirect rule.
//
// The value is the literal ASCII "NX" (0x4E58). It is NOT a secret —
// its purpose is identification, not authentication. A local attacker
// with CAP_NET_ADMIN could set the same mark on their own traffic to
// escape interception, but they could already disable the agent, so
// no security property is lost.
const AgentSOMark = 0x4E58

// markControl is the [net.Dialer.Control] callback that sets SO_MARK
// on a freshly-created socket file descriptor, before connect() is
// called. The kernel applies the mark to every outbound packet on the
// socket for its lifetime.
func markControl(network, address string, c syscall.RawConn) error {
	var sockerr error
	err := c.Control(func(fd uintptr) {
		sockerr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, AgentSOMark)
	})
	if err != nil {
		return fmt.Errorf("SO_MARK control: %w", err)
	}
	return sockerr
}

// MarkedDialer returns a [net.Dialer] that stamps every outbound
// socket it creates with [AgentSOMark]. Call sites that already
// have a [net.Dialer] for their own reasons (timeouts, KeepAlive,
// etc.) should compose by setting `Control: markControl` on their
// existing dialer rather than swapping it out wholesale.
func MarkedDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   markControl,
	}
}

// MarkedTransport returns an [http.Transport] whose DialContext
// produces SO_MARK-stamped sockets. Use it for the agent's outbound
// HTTP clients (enrollment, relay, updater, thingclient HTTP
// fallback) so their egress is excluded from the NEXUS_AGENT chain.
//
// The transport is configured with reasonable production defaults
// (keep-alive, connection pooling). Callers that need to override
// any field should wrap with a custom transport that re-uses the
// returned transport's DialContext.
func MarkedTransport() *http.Transport {
	d := MarkedDialer()
	return &http.Transport{
		DialContext:           d.DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}
