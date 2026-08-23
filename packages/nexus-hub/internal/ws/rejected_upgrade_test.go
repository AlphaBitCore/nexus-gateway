package ws

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// rejectingServer is a WS server whose auth always fails, with its WARN output
// captured so the refusal itself can be asserted on.
func rejectingServer() (*Server, *bytes.Buffer) {
	var buf bytes.Buffer
	pool := NewPool(opsmetrics.NewRegistry(prometheus.NewRegistry()), nullLogger())
	srv := newServerWithDeps(pool, &fakeManager{}, &fakeValidator{},
		"test-hub", testServiceToken, nil, true,
		slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	return srv, &buf
}

// A refused upgrade has to name the caller who was refused.
//
// What it named before was r.RemoteAddr, and the Hub sits behind an ALB and
// nginx — so every refusal reported the loopback address the proxy connects
// from. Production carried 3999 of them in two hours, all saying 127.0.0.1, and
// working out that they came from ONE external agent retrying with a token the
// Hub rejects took a packet capture on the box. An operator should not need one.
func TestRejectedUpgrade_NamesTheForwardedClientNotTheProxy(t *testing.T) {
	srv, buf := rejectingServer()

	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.RemoteAddr = "127.0.0.1:41234" // what nginx connects from
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 172.31.0.171")
	req.Header.Set("User-Agent", "Go-http-client/1.1")
	srv.HandleUpgrade(httptest.NewRecorder(), req)

	out := buf.String()
	if !strings.Contains(out, `"client":"203.0.113.7"`) {
		t.Errorf("the refusal does not name the originating client; an operator sees only the "+
			"proxy and cannot tell an external caller from a local misconfiguration:\n%s", out)
	}
	if !strings.Contains(out, `"user_agent":"Go-http-client/1.1"`) {
		t.Errorf("the refusal drops the user agent, which is what identified this caller as an "+
			"agent rather than a browser:\n%s", out)
	}
	// The proxy hop stays, because it is how you tell a direct connection from a
	// forwarded one.
	if !strings.Contains(out, `"remote_addr":"127.0.0.1:41234"`) {
		t.Errorf("the proxy address was dropped:\n%s", out)
	}
}

// With no proxy in front there is nothing to prefer, and the connection address
// IS the client.
func TestRejectedUpgrade_FallsBackToTheConnectionAddress(t *testing.T) {
	srv, buf := rejectingServer()

	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.RemoteAddr = "198.51.100.9:5555"
	srv.HandleUpgrade(httptest.NewRecorder(), req)

	if !strings.Contains(buf.String(), `"client":"198.51.100.9:5555"`) {
		t.Errorf("without X-Forwarded-For the connection address is the client:\n%s", buf.String())
	}
}

// A caller retrying forever costs one line, not one per attempt.
//
// At ~33 refusals a minute the previous behaviour drowned the WARN stream the
// deploy smoke reads, which is how a standing failure stayed invisible for
// hours. The count has to survive, though — a refusal nobody can quantify is a
// refusal nobody prioritises.
func TestRejectedUpgrade_RepeatsAreCountedNotRepeated(t *testing.T) {
	srv, buf := rejectingServer()

	const attempts = 50
	for range attempts {
		req := httptest.NewRequest(http.MethodGet, "/ws", nil)
		req.RemoteAddr = "127.0.0.1:41234"
		req.Header.Set("X-Forwarded-For", "203.0.113.42")
		srv.HandleUpgrade(httptest.NewRecorder(), req)
	}

	lines := strings.Count(buf.String(), `"msg":"ws authenticate failed"`)
	if lines != 1 {
		t.Errorf("%d attempts produced %d log lines, want 1 — this is the flood that hid a "+
			"standing production failure inside the stream the deploy smoke reads",
			attempts, lines)
	}
	if !strings.Contains(buf.String(), `"attempts":1`) {
		t.Errorf("the line carries no attempt count, so nobody can tell one mistake from a "+
			"caller stuck in a retry loop:\n%s", buf.String())
	}

	// A DIFFERENT client is a different fact and must not be suppressed by the
	// first one's counter.
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.99")
	srv.HandleUpgrade(httptest.NewRecorder(), req)
	if strings.Count(buf.String(), `"msg":"ws authenticate failed"`) != 2 {
		t.Errorf("a second, distinct client was suppressed by the first client's count:\n%s",
			buf.String())
	}
}
