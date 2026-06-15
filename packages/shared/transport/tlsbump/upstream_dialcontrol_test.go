package tlsbump

import (
	"syscall"
	"testing"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// TestUpstreamDialControl pins that the MITM upstream dialer applies the
// process-wide dial control hook. The Linux agent installs an SO_MARK setter
// there so its upstream forward escapes its own iptables REDIRECT; if
// upstreamDialControl failed to delegate, the upstream would self-loop back
// into the agent (the bug this guards against). It must also be a safe no-op
// when no hook is installed (compliance-proxy / ai-gateway / macOS / Windows).
func TestUpstreamDialControl(t *testing.T) {
	// Restore process-wide state regardless of outcome.
	defer nexushttp.SetGlobalDialControl(nil)

	// No hook installed → no-op, returns nil.
	nexushttp.SetGlobalDialControl(nil)
	if err := upstreamDialControl("tcp", "1.2.3.4:443", nil); err != nil {
		t.Fatalf("no hook installed: want nil, got %v", err)
	}

	// Hook installed → upstreamDialControl must delegate to it verbatim.
	called := false
	var gotNetwork, gotAddr string
	nexushttp.SetGlobalDialControl(func(network, address string, _ syscall.RawConn) error {
		called = true
		gotNetwork, gotAddr = network, address
		return nil
	})
	if err := upstreamDialControl("tcp", "5.6.7.8:443", nil); err != nil {
		t.Fatalf("delegate: unexpected err %v", err)
	}
	if !called {
		t.Fatal("upstreamDialControl did not delegate to the installed global hook — MITM upstream would not be SO_MARK'd and would self-loop on the Linux agent")
	}
	if gotNetwork != "tcp" || gotAddr != "5.6.7.8:443" {
		t.Errorf("hook received (%q,%q), want (tcp, 5.6.7.8:443)", gotNetwork, gotAddr)
	}
}
