package thingclient

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// A refused credential is not a blip, and the client must stop treating it like
// one.
//
// Measured on production before this existed: one agent whose Thing had been
// removed held ~33 rejected upgrades a minute against the Hub, for hours,
// while appearing to its owner to be running. Nothing backed off, nothing gave
// up, nothing said so.
//
// The test is a rate comparison rather than an assertion about one attempt,
// because the defect is a rate: a permanently-rejecting Hub must produce far
// fewer attempts over a fixed window than an unreachable one, AND the
// unreachable case must still recover fast — a fix that slows both down trades
// this defect for an outage after every Hub restart.
func TestReconnect_AuthRejectionIsNotRetriedLikeAnOutage(t *testing.T) {
	const window = 700 * time.Millisecond

	attempts := func(t *testing.T, status int) int64 {
		t.Helper()
		// Count ONLY the WebSocket upgrade attempts. The first version counted
		// every request to the server, which the HTTP-fallback path also hits —
		// so the "unreachable" arm was inflated by a different mechanism and the
		// comparison passed with the fix removed. The defect is a rate of
		// UPGRADE attempts; that is what has to be counted.
		var n atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/ws") {
				n.Add(1)
			}
			w.WriteHeader(status)
		}))
		defer srv.Close()

		c := clientAgainst(t, srv.URL)
		ctx, cancel := context.WithTimeout(context.Background(), window)
		defer cancel()
		c.runLoop(ctx)
		return n.Load()
	}

	rejected := attempts(t, http.StatusUnauthorized)
	unreachable := attempts(t, http.StatusServiceUnavailable)

	if rejected == 0 {
		t.Fatal("the client never dialled at all; this measured nothing")
	}
	if rejected >= unreachable {
		t.Errorf("a permanently-rejecting Hub drew %d attempts and an unavailable one %d — a "+
			"refused credential must be retried far less often than a transient failure, or a "+
			"stale agent hammers the auth path forever", rejected, unreachable)
	}
	if unreachable < 2 {
		t.Errorf("an unavailable Hub drew only %d attempt(s) in %v — the transient path must "+
			"still recover quickly, or every Hub restart becomes an outage", unreachable, window)
	}
}

// The classification itself: a 401 handshake is marked, everything else is not.
// Without this the loop cannot tell them apart and the rate fix has nothing to
// key on.
func TestConnectWS_A401IsMarkedAsAnAuthRejection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"unauthorized", http.StatusUnauthorized, true},
		{"service unavailable", http.StatusServiceUnavailable, false},
		{"internal error", http.StatusInternalServerError, false},
		{"forbidden is not an auth rejection here", http.StatusForbidden, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			c := clientAgainst(t, srv.URL)
			err := c.connectWS(context.Background())
			if err == nil {
				t.Fatal("the handshake succeeded against a server that refused it")
			}
			if got := errors.Is(err, ErrAuthRejected); got != tc.wantErr {
				t.Errorf("errors.Is(err, ErrAuthRejected) = %v, want %v for status %d (err: %v)",
					got, tc.wantErr, tc.status, err)
			}
		})
	}
}

// clientAgainst builds a client pointed at a test server, with the reconnect
// backoffs compressed so a rate comparison fits in a test.
func clientAgainst(t *testing.T, httpURL string) *Client {
	t.Helper()
	c, err := New(Config{
		HubURL:                  "ws" + httpURL[len("http"):] + "/ws",
		ThingType:               "test-thing",
		ThingID:                 "thing-auth-probe",
		Token:                   "test-token",
		Logger:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
		MetricsRegisterer:       prometheus.NewRegistry(),
		MetricsNamespace:        "test",
		ReconnectInitialBackoff: 20 * time.Millisecond,
		ReconnectMaxBackoff:     40 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return c
}
