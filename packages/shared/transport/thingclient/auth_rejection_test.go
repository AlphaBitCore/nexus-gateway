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
	// The window is anchored on observed work, not on the wall clock.
	//
	// A flat 700ms for both arms makes the test a
	// measure of the machine as much as of the client: under `go test ./...`,
	// where every package in this module runs at once, a single dial can eat
	// the whole budget, no upgrade is recorded, and the vacuity guard below
	// fires with `rejected == 0`. Green in isolation, red under load — which is
	// the orchestration showing through the test rather than a defect in the
	// client.
	//
	// Raising the constant would only move the cliff, and would pay for it in
	// every run. Instead the unreachable arm runs until it has drawn a known
	// number of attempts, and the time IT took becomes the window the rejecting
	// arm is given. A slow machine lengthens both arms together, so the
	// comparison the test actually makes — a rate against a rate — holds at any
	// speed, and the hard cap only ever fires when the client has stopped
	// dialling altogether, which is itself the finding.
	const (
		pace = 5                // attempts that define the window
		cap_ = 15 * time.Second // only reached if the client stops dialling
	)

	// serve counts WebSocket upgrade attempts against a server answering
	// `status`, stopping at `stopAt` attempts or when `budget` elapses.
	//
	// Count ONLY the upgrades. The first version counted every request, which
	// the HTTP-fallback path also makes — so the "unreachable" arm was inflated
	// by a different mechanism and the comparison passed with the fix removed.
	serve := func(t *testing.T, status int, stopAt int64, budget time.Duration) (int64, time.Duration) {
		t.Helper()
		var n atomic.Int64
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/ws") {
				// Count first, unconditionally. Folding the increment into the
				// stop condition short-circuits it away on the arm that has no
				// stop, and that arm then reports zero attempts however many it
				// made — indistinguishable from a client that never dialled.
				seen := n.Add(1)
				if stopAt > 0 && seen >= stopAt {
					cancel()
				}
			}
			w.WriteHeader(status)
		}))
		defer srv.Close()

		c := clientAgainst(t, srv.URL)
		start := time.Now()
		c.runLoop(ctx)
		return n.Load(), time.Since(start)
	}

	unreachable, window := serve(t, http.StatusServiceUnavailable, pace, cap_)
	if unreachable < 2 {
		t.Fatalf("an unavailable Hub drew only %d attempt(s) in %v — the transient path must "+
			"still recover quickly, or every Hub restart becomes an outage", unreachable, window)
	}

	rejected, _ := serve(t, http.StatusUnauthorized, 0, window)
	if rejected == 0 {
		t.Fatalf("the client never dialled at all in %v, a window in which an unreachable Hub "+
			"drew %d attempts; this measured nothing", window, unreachable)
	}
	if rejected >= unreachable {
		t.Errorf("a permanently-rejecting Hub drew %d attempts and an unavailable one %d in %v — "+
			"a refused credential must be retried far less often than a transient failure, or a "+
			"stale agent hammers the auth path forever", rejected, unreachable, window)
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
