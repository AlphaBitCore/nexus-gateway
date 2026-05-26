package jwtverifier_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jwtverifier "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/jwt"
)

// testEvent mirrors the wire shape of nexus.auth.revocation events without
// importing the control-plane revocation package (which depends on shared and
// would create a module cycle).
type testEvent struct {
	EventID         string    `json:"event_id"`
	RevokedAt       time.Time `json:"revoked_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	Scope           string    `json:"scope"`
	TargetJTI       string    `json:"target_jti,omitempty"`
	TargetUserID    string    `json:"target_user_id,omitempty"`
	TargetDeviceID  string    `json:"target_device_id,omitempty"`
	TargetSessionID string    `json:"target_session_id,omitempty"`
	Reason          string    `json:"reason,omitempty"`
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestMQRevocationChecker_UserCutoffRevokesOlderTokens(t *testing.T) {
	t.Parallel()

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{})
	ev := testEvent{
		EventID:      "evt-1",
		Scope:        "user",
		TargetUserID: "u1",
		RevokedAt:    time.Now().Add(-time.Minute),
		ExpiresAt:    time.Now().Add(time.Hour),
		Reason:       "test",
	}
	if err := ch.HandleMessage(context.Background(), mustMarshal(t, ev)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	revoked, err := ch.IsRevoked(context.Background(), &jwtverifier.Claims{
		Subject:  "u1",
		IssuedAt: time.Now().Add(-2 * time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("IsRevoked(old): %v", err)
	}
	if !revoked {
		t.Fatalf("expected revoked (iat < revokedAt)")
	}

	revoked, err = ch.IsRevoked(context.Background(), &jwtverifier.Claims{
		Subject:  "u1",
		IssuedAt: time.Now().Add(1 * time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("IsRevoked(new): %v", err)
	}
	if revoked {
		t.Fatalf("tokens issued after revokedAt must be allowed")
	}
}

// TestMQRevocationChecker_JTIBloom_FalsePositiveFallsBackToIntrospect exercises
// the bloom disambiguation branch: a JTI that the bloom claims "maybe" present
// but that is NOT in the exact byJTI set must trigger an introspect round-trip.
// When introspect reports active=true the token is allowed through.
//
// To produce a deterministic false positive we use the exported test seam to
// inject a raw bloom entry without adding it to byJTI.
func TestMQRevocationChecker_JTIBloom_FalsePositiveFallsBackToIntrospect(t *testing.T) {
	t.Parallel()

	// Track that introspect was actually called -- the whole point of this
	// test is that the bloom-false-positive branch defers to introspect rather
	// than blanket-allowing or blanket-denying.
	var called atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("introspect got method %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Errorf("introspect Content-Type = %q, want application/x-www-form-urlencoded", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		if r.Form.Get("token") != "raw-jwt-value" {
			t.Errorf("token form = %q, want raw-jwt-value", r.Form.Get("token"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"active":true}`))
	}))
	t.Cleanup(server.Close)

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{
		IntrospectURL: server.URL,
	})

	// Seed an unrelated JTI through the real event path so byJTI is non-empty.
	seed := testEvent{
		EventID:   "evt-seed",
		Scope:     "jti",
		TargetJTI: "Z",
		RevokedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := ch.HandleMessage(context.Background(), mustMarshal(t, seed)); err != nil {
		t.Fatalf("HandleMessage seed: %v", err)
	}

	// Inject a bloom-only entry (not present in byJTI) -- simulates the
	// bloom-false-positive case the disambiguation branch is designed for.
	jwtverifier.SeedBloomOnly(ch, "Y")

	revoked, err := ch.IsRevoked(context.Background(), &jwtverifier.Claims{
		JTI:      "Y",
		Subject:  "any",
		IssuedAt: time.Now().Unix(),
		Raw:      "raw-jwt-value",
	})
	if err != nil {
		t.Fatalf("IsRevoked: %v", err)
	}
	if revoked {
		t.Fatalf("expected allowed (introspect active=true), got revoked")
	}
	if called.Load() != 1 {
		t.Fatalf("introspect call count = %d, want 1", called.Load())
	}
}

func TestMQRevocationChecker_StrictModeOnDisconnect(t *testing.T) {
	t.Parallel()

	// Introspect stub so the strict-mode IsRevoked path does not explode when
	// exercised.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"active":true}`))
	}))
	t.Cleanup(server.Close)

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{
		IntrospectURL:     server.URL,
		DisconnectTimeout: 100 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// subscribe blocks until ctx done, mimicking a connected-but-silent MQ.
	subscribe := func(sctx context.Context, _ func(context.Context, []byte) error) error {
		<-sctx.Done()
		return sctx.Err()
	}

	done := make(chan error, 1)
	go func() {
		done <- ch.StartConsumer(ctx, subscribe)
	}()

	// Poll until strict mode flips on. DisconnectTimeout is 100ms and the
	// ticker runs at DisconnectTimeout/3, so 500ms is a generous budget.
	deadline := time.Now().Add(500 * time.Millisecond)
	for !jwtverifier.StrictLoad(ch) {

		if time.Now().After(deadline) {
			t.Fatalf("strict mode did not engage within deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A fresh message must flip strict back off.
	ev := testEvent{
		EventID:      "evt-wake",
		Scope:        "user",
		TargetUserID: "u-wake",
		RevokedAt:    time.Now(),
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	if err := ch.HandleMessage(context.Background(), mustMarshal(t, ev)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if jwtverifier.StrictLoad(ch) {
		t.Fatalf("strict mode must clear after a successful HandleMessage")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("StartConsumer returned %v, want nil or context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("StartConsumer did not return after cancel")
	}
}

// TestMQRevocationChecker_ConcurrentIngestDuringIntrospect verifies that
// HandleMessage (which takes a write lock) is never blocked by a concurrent
// IsRevoked call that is executing an HTTP introspect round-trip. The read
// lock must be released before the HTTP call.
func TestMQRevocationChecker_ConcurrentIngestDuringIntrospect(t *testing.T) {
	t.Parallel()

	// unblock is closed by the main goroutine to release the introspect handler.
	// releaseOnce ensures we never close it twice (test failure path + normal path).
	unblock := make(chan struct{})
	var releaseOnce sync.Once
	releaseIntrospect := func() { releaseOnce.Do(func() { close(unblock) }) }
	t.Cleanup(releaseIntrospect) // safety: release if the test panics or fails early

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the test releases us.
		<-unblock
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"active":true}`))
	}))
	t.Cleanup(server.Close)

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{
		IntrospectURL: server.URL,
	})

	// Seed the bloom filter only (not byJTI) so IsRevoked triggers introspect.
	const targetJTI = "slow-jti"
	jwtverifier.SeedBloomOnly(ch, targetJTI)

	// Kick off IsRevoked in a goroutine; it will block inside the HTTP call.
	isRevokedDone := make(chan error, 1)
	go func() {
		_, err := ch.IsRevoked(context.Background(), &jwtverifier.Claims{
			JTI:     targetJTI,
			Subject: "any",
			Raw:     "raw-jwt-for-introspect",
		})
		isRevokedDone <- err
	}()

	// Give the goroutine time to enter the HTTP handler and block.
	time.Sleep(30 * time.Millisecond)

	// HandleMessage must NOT block waiting for the read lock held by IsRevoked;
	// the lock must already be released before the HTTP call.
	handleDone := make(chan error, 1)
	go func() {
		ev := testEvent{
			EventID:      "evt-concurrent",
			Scope:        "user",
			TargetUserID: "u-concurrent",
			RevokedAt:    time.Now(),
			ExpiresAt:    time.Now().Add(time.Hour),
		}
		handleDone <- ch.HandleMessage(context.Background(), mustMarshal(t, ev))
	}()

	// HandleMessage must complete well within 200ms even though the introspect
	// HTTP handler is still blocked.
	select {
	case err := <-handleDone:
		if err != nil {
			t.Fatalf("HandleMessage returned error: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("HandleMessage blocked for >200ms - read lock was not released before introspect HTTP call")
	}

	// Release the introspect handler and wait for IsRevoked to finish.
	releaseIntrospect()
	select {
	case err := <-isRevokedDone:
		if err != nil {
			t.Fatalf("IsRevoked returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("IsRevoked did not return after introspect was unblocked")
	}
}

func TestMQRevocationChecker_ReplayCatchup_AppliesMissedEvents(t *testing.T) {
	t.Parallel()

	now := time.Now()
	userCutoff := now.Add(-time.Minute)
	sessionCutoff := now.Add(-30 * time.Second)

	replayHits := atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		replayHits.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("replay got method %s, want GET", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("replay Authorization = %q, want Bearer secret", got)
		}
		q, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			t.Fatalf("ParseQuery: %v", err)
		}
		if q.Get("since") != "0" {
			t.Errorf("since = %q, want 0", q.Get("since"))
		}
		if q.Get("limit") != "1000" {
			t.Errorf("limit = %q, want 1000", q.Get("limit"))
		}

		events := []testEvent{
			{
				EventID:      "evt-a",
				Scope:        "user",
				TargetUserID: "u42",
				RevokedAt:    userCutoff,
				ExpiresAt:    now.Add(time.Hour),
			},
			{
				EventID:         "evt-b",
				Scope:           "session",
				TargetSessionID: "sess-9",
				RevokedAt:       sessionCutoff,
				ExpiresAt:       now.Add(time.Hour),
			},
		}
		resp := map[string]any{
			"events": events,
			"lastId": int64(42),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(server.Close)

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{
		ReplayURL:        server.URL + "/replay",
		ReplayAuthHeader: "Bearer secret",
	})

	if err := ch.RunCatchup(context.Background()); err != nil {
		t.Fatalf("RunCatchup: %v", err)
	}
	if got := replayHits.Load(); got != 1 {
		t.Fatalf("replay hits = %d, want 1", got)
	}
	if got := jwtverifier.LastIDLoad(ch); got != 42 {
		t.Fatalf("lastID = %d, want 42", got)
	}

	// Older user token must be revoked.
	revoked, err := ch.IsRevoked(context.Background(), &jwtverifier.Claims{
		Subject:  "u42",
		IssuedAt: userCutoff.Add(-10 * time.Second).Unix(),
	})
	if err != nil {
		t.Fatalf("IsRevoked(u42 old): %v", err)
	}
	if !revoked {
		t.Fatalf("u42 token older than cutoff must be revoked")
	}

	// Older session token must be revoked.
	revoked, err = ch.IsRevoked(context.Background(), &jwtverifier.Claims{
		Subject:   "someone",
		SessionID: "sess-9",
		IssuedAt:  sessionCutoff.Add(-5 * time.Second).Unix(),
	})
	if err != nil {
		t.Fatalf("IsRevoked(sess-9 old): %v", err)
	}
	if !revoked {
		t.Fatalf("sess-9 token older than cutoff must be revoked")
	}

	// Newer tokens post-cutoff must pass.
	revoked, err = ch.IsRevoked(context.Background(), &jwtverifier.Claims{
		Subject:  "u42",
		IssuedAt: now.Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("IsRevoked(u42 new): %v", err)
	}
	if revoked {
		t.Fatalf("u42 token newer than cutoff must be allowed")
	}
}
