package wiring

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
)

// The proxy write paths lift the connection write deadline to the upstream
// budget before writing a response, because Go arms the server write deadline
// when the request header is read: a non-stream inference that spends minutes
// upstream is already past a flat server.writeTimeout by the time its body is
// written. That lift only reaches the connection if every ResponseWriter
// wrapper between the handler and the socket implements Unwrap — that is what
// http.ResponseController walks.
//
// The proxy's own tests prove the handler arms the budget, but they drive the
// handler directly against a recorder, so they say nothing about the wrappers
// the mounted chain actually interposes. These tests close that gap: they run
// the real MountCoreRoutes chain over a real TCP connection, so deleting Unwrap
// from any wrapper in the chain (or adding a new wrapper without one) fails
// here.
const (
	// Shorter than probeUpstreamDelay so the flat server deadline is
	// certainly blown by the time the probe writes.
	probeServerWriteTimeout = 250 * time.Millisecond
	// Stands in for a slow upstream inference.
	probeUpstreamDelay = 750 * time.Millisecond

	probeBody = `{"object":"chat.completion","choices":[{"message":{"content":"hi"}}]}`

	probePathExtended = "/__test/write-deadline/extended"
	probePathFlat     = "/__test/write-deadline/flat"
)

// registerWriteDeadlineProbes mounts two leaf handlers on the mux that
// MountCoreRoutes wraps, so requests to them traverse the production
// middleware chain exactly as /v1/chat/completions does.
func registerWriteDeadlineProbes(mux *http.ServeMux) {
	// Mirrors the general non-stream proxy write path: lift the deadline to
	// the upstream budget, spend a long time upstream, then write.
	mux.HandleFunc("POST "+probePathExtended, func(w http.ResponseWriter, _ *http.Request) {
		rc := http.NewResponseController(w)
		_ = rc.SetWriteDeadline(time.Now().Add(specutil.ActiveConfig().Timeout))
		time.Sleep(probeUpstreamDelay)
		_, _ = w.Write([]byte(probeBody))
	})

	// Control arm: identical, minus the lift. Proves the harness genuinely
	// enforces server.writeTimeout, so a pass on the extended arm means the
	// lift landed rather than the deadline never having bitten at all.
	mux.HandleFunc("POST "+probePathFlat, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(probeUpstreamDelay)
		_, _ = w.Write([]byte(probeBody))
	})
}

// newProbeServer starts the real mounted chain on a real listener with a
// server write timeout far shorter than the probe's upstream delay.
func newProbeServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(getSharedCoreHandler(t))
	srv.Config.WriteTimeout = probeServerWriteTimeout
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// probePost issues one request on its own connection. Keep-alives are disabled
// so the control arm's poisoned connection can never be reused by the other
// arm.
func probePost(t *testing.T, url string) (string, error) {
	t.Helper()
	client := &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
		Timeout:   probeUpstreamDelay + 5*time.Second,
	}
	defer client.CloseIdleConnections()

	resp, err := client.Post(url, "application/json", http.NoBody)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

// TestMountedChain_NonStreamWriteAfterUpstreamBudgetReachesClient is the guard.
// A response written long after server.writeTimeout must still reach the client
// through the mounted chain — that response is a completed inference the caller
// has already been billed for, so losing it to a stale write deadline is the
// worst outcome on this path.
func TestMountedChain_NonStreamWriteAfterUpstreamBudgetReachesClient(t *testing.T) {
	srv := newProbeServer(t)

	body, err := probePost(t, srv.URL+probePathExtended)
	if err != nil {
		t.Fatalf("a response written %v after a %v server.writeTimeout did not reach the client: %v\n"+
			"the write-deadline lift did not reach the connection — some ResponseWriter wrapper in the mounted chain is missing Unwrap(), so http.ResponseController could not walk to the real connection",
			probeUpstreamDelay, probeServerWriteTimeout, err)
	}
	if body != probeBody {
		t.Fatalf("body: got %q, want %q — the completed inference was truncated in flight", body, probeBody)
	}
}

// TestMountedChain_WithoutLiftFlatWriteTimeoutSeversResponse pins the control.
// Without the lift the same response must be lost, which is what makes the
// test above meaningful: it proves the flat server.writeTimeout is real and
// actually bites at probeUpstreamDelay. If this ever starts passing, the guard
// above has gone vacuous and proves nothing.
func TestMountedChain_WithoutLiftFlatWriteTimeoutSeversResponse(t *testing.T) {
	srv := newProbeServer(t)

	body, err := probePost(t, srv.URL+probePathFlat)
	if err == nil && body == probeBody {
		t.Fatalf("a response written %v after a %v server.writeTimeout reached the client intact; "+
			"server.writeTimeout is not being enforced, so the sibling test's pass would not prove the deadline lift works",
			probeUpstreamDelay, probeServerWriteTimeout)
	}
}
