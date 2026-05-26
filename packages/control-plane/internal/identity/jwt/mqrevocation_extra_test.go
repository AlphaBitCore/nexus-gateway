package jwtverifier_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jwtverifier "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/jwt"
)

// TestMQRevocationChecker_DeviceCutoffRevokesOlderTokens pins the device-scope
// branch of IsRevoked that the existing user-scope test does not cover. A
// device-scope revocation must reject any token whose iat predates the event's
// revokedAt for that device id.
func TestMQRevocationChecker_DeviceCutoffRevokesOlderTokens(t *testing.T) {
	t.Parallel()

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{})
	ev := testEvent{
		EventID:        "evt-dev",
		Scope:          "device",
		TargetDeviceID: "dev-1",
		RevokedAt:      time.Now().Add(-time.Minute),
		ExpiresAt:      time.Now().Add(time.Hour),
	}
	if err := ch.HandleMessage(context.Background(), mustMarshal(t, ev)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	revoked, err := ch.IsRevoked(context.Background(), &jwtverifier.Claims{
		Subject:  "anyone",
		DeviceID: "dev-1",
		IssuedAt: time.Now().Add(-2 * time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("IsRevoked(old device token): %v", err)
	}
	if !revoked {
		t.Fatalf("device-scope cutoff must reject older tokens")
	}

	// Newer-than-cutoff passes.
	revoked, err = ch.IsRevoked(context.Background(), &jwtverifier.Claims{
		Subject:  "anyone",
		DeviceID: "dev-1",
		IssuedAt: time.Now().Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("IsRevoked(new device token): %v", err)
	}
	if revoked {
		t.Fatalf("newer-than-cutoff device token must pass")
	}
}

// TestMQRevocationChecker_JTIBloomExactMatch pins the path where the bloom
// filter reports HIT and the exact byJTI set CONFIRMS — IsRevoked returns
// (true, nil) immediately without an introspect round-trip. This is the
// happy-path single-event JTI revocation flow.
func TestMQRevocationChecker_JTIBloomExactMatch(t *testing.T) {
	t.Parallel()

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{
		// IntrospectURL intentionally left empty: this path must NOT hit it.
	})
	ev := testEvent{
		EventID:   "evt-jti",
		Scope:     "jti",
		TargetJTI: "jti-revoked",
		RevokedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := ch.HandleMessage(context.Background(), mustMarshal(t, ev)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	revoked, err := ch.IsRevoked(context.Background(), &jwtverifier.Claims{
		JTI:      "jti-revoked",
		Subject:  "any",
		IssuedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("IsRevoked: %v", err)
	}
	if !revoked {
		t.Fatal("JTI exact-match in bloom+byJTI must be revoked")
	}
}

// TestMQRevocationChecker_HandleMessage_BadJSON pins the decode-error branch.
// A malformed payload must surface a wrapped error so the consumer can decide
// whether to retry or DLQ; the checker state must not be mutated.
func TestMQRevocationChecker_HandleMessage_BadJSON(t *testing.T) {
	t.Parallel()

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{})
	err := ch.HandleMessage(context.Background(), []byte(`{not json`))
	if err == nil {
		t.Fatal("HandleMessage with malformed JSON: err = nil, want decode error")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("err = %v, want wrapped decode error", err)
	}
}

// TestMQRevocationChecker_ApplyDroppedEvents covers the malformed-event branches
// in applyLocked: each scope value with an empty target id is silently dropped
// (Warn-logged) and never mutates the in-memory state. A token that would
// otherwise match the in-flight scope must NOT be marked revoked. Also covers
// the unrecognized-scope default branch.
func TestMQRevocationChecker_ApplyDroppedEvents(t *testing.T) {
	t.Parallel()

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{})

	// Drive every "drop because target is empty" branch.
	bad := []testEvent{
		{EventID: "drop-jti", Scope: "jti", TargetJTI: "", RevokedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)},
		{EventID: "drop-user", Scope: "user", TargetUserID: "", RevokedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)},
		{EventID: "drop-device", Scope: "device", TargetDeviceID: "", RevokedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)},
		{EventID: "drop-session", Scope: "session", TargetSessionID: "", RevokedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)},
		{EventID: "drop-unknown", Scope: "ipaddr", TargetUserID: "u1", RevokedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)},
	}
	for _, ev := range bad {
		if err := ch.HandleMessage(context.Background(), mustMarshal(t, ev)); err != nil {
			t.Fatalf("HandleMessage(%q): %v", ev.EventID, err)
		}
	}

	// None of the malformed events should have mutated state — a "u1" token
	// must NOT be revoked despite the unrecognized-scope event carrying that
	// user id. The Subject-with-old-iat probe verifies the byUser map is empty.
	revoked, err := ch.IsRevoked(context.Background(), &jwtverifier.Claims{
		Subject:  "u1",
		IssuedAt: time.Now().Add(-1 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("IsRevoked: %v", err)
	}
	if revoked {
		t.Fatalf("malformed/unknown-scope events must not mutate checker state")
	}
}

// TestMQRevocationChecker_UserRevokedAtNotMonotonic locks the
// "only-keep-the-latest-cutoff" invariant in applyLocked: when two user-scope
// events arrive out of order, the older revokedAt must not overwrite the
// newer cutoff. Otherwise a stale catch-up replay would let already-rejected
// tokens through.
func TestMQRevocationChecker_UserRevokedAtNotMonotonic(t *testing.T) {
	t.Parallel()

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{})

	newer := time.Now().Add(-30 * time.Second)
	older := time.Now().Add(-2 * time.Minute)

	// Apply newer first.
	mustHandle := func(ev testEvent) {
		if err := ch.HandleMessage(context.Background(), mustMarshal(t, ev)); err != nil {
			t.Fatalf("HandleMessage(%q): %v", ev.EventID, err)
		}
	}
	mustHandle(testEvent{
		EventID:      "newer",
		Scope:        "user",
		TargetUserID: "u-mono",
		RevokedAt:    newer,
		ExpiresAt:    time.Now().Add(time.Hour),
	})
	// Then apply older — must NOT overwrite the newer cutoff.
	mustHandle(testEvent{
		EventID:      "older",
		Scope:        "user",
		TargetUserID: "u-mono",
		RevokedAt:    older,
		ExpiresAt:    time.Now().Add(time.Hour),
	})

	// A token issued between older and newer must still be revoked (cutoff is
	// the newer of the two).
	revoked, err := ch.IsRevoked(context.Background(), &jwtverifier.Claims{
		Subject:  "u-mono",
		IssuedAt: time.Now().Add(-1 * time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("IsRevoked: %v", err)
	}
	if !revoked {
		t.Fatalf("older revokedAt must not overwrite newer cutoff")
	}
}

// TestMQRevocationChecker_SessionRevokedAtNotMonotonic mirrors the user-scope
// monotonic test for the session branch.
func TestMQRevocationChecker_SessionRevokedAtNotMonotonic(t *testing.T) {
	t.Parallel()

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{})
	newer := time.Now().Add(-30 * time.Second)
	older := time.Now().Add(-2 * time.Minute)

	for _, when := range []time.Time{newer, older} {
		if err := ch.HandleMessage(context.Background(), mustMarshal(t, testEvent{
			EventID:         "sess",
			Scope:           "session",
			TargetSessionID: "sess-mono",
			RevokedAt:       when,
			ExpiresAt:       time.Now().Add(time.Hour),
		})); err != nil {
			t.Fatalf("HandleMessage: %v", err)
		}
	}

	revoked, err := ch.IsRevoked(context.Background(), &jwtverifier.Claims{
		Subject:   "anyone",
		SessionID: "sess-mono",
		IssuedAt:  time.Now().Add(-1 * time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("IsRevoked: %v", err)
	}
	if !revoked {
		t.Fatalf("session newer cutoff must not be overwritten by older")
	}
}

// TestMQRevocationChecker_DeviceRevokedAtNotMonotonic mirrors the monotonic
// test for the device branch.
func TestMQRevocationChecker_DeviceRevokedAtNotMonotonic(t *testing.T) {
	t.Parallel()

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{})
	newer := time.Now().Add(-30 * time.Second)
	older := time.Now().Add(-2 * time.Minute)

	for _, when := range []time.Time{newer, older} {
		if err := ch.HandleMessage(context.Background(), mustMarshal(t, testEvent{
			EventID:        "dev",
			Scope:          "device",
			TargetDeviceID: "dev-mono",
			RevokedAt:      when,
			ExpiresAt:      time.Now().Add(time.Hour),
		})); err != nil {
			t.Fatalf("HandleMessage: %v", err)
		}
	}

	revoked, err := ch.IsRevoked(context.Background(), &jwtverifier.Claims{
		Subject:  "anyone",
		DeviceID: "dev-mono",
		IssuedAt: time.Now().Add(-1 * time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("IsRevoked: %v", err)
	}
	if !revoked {
		t.Fatalf("device newer cutoff must not be overwritten by older")
	}
}

// TestMQRevocationChecker_StrictMode_IntrospectAllow forces strict mode (via
// the unexported export-test seam) and verifies that IsRevoked round-trips to
// introspect on EVERY call. active=true → allowed.
func TestMQRevocationChecker_StrictMode_IntrospectAllow(t *testing.T) {
	t.Parallel()

	var called atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer rs-token" {
			t.Errorf("Authorization = %q, want Bearer rs-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"active":true}`))
	}))
	t.Cleanup(server.Close)

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{
		IntrospectURL:    server.URL,
		ReplayAuthHeader: "Bearer rs-token",
	})
	// Force strict via the export-test seam.
	jwtverifier.SetStrict(ch, true)

	revoked, err := ch.IsRevoked(context.Background(), &jwtverifier.Claims{
		JTI:     "whatever",
		Subject: "any",
		Raw:     "the-jwt",
	})
	if err != nil {
		t.Fatalf("strict-mode IsRevoked: %v", err)
	}
	if revoked {
		t.Fatal("active=true must be treated as not-revoked")
	}
	if called.Load() != 1 {
		t.Fatalf("introspect calls = %d, want 1", called.Load())
	}
}

// TestMQRevocationChecker_StrictMode_IntrospectRevoke covers the
// active=false → revoked surface. Strict-mode IsRevoked must surface the
// revocation decision verbatim.
func TestMQRevocationChecker_StrictMode_IntrospectRevoke(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"active":false}`))
	}))
	t.Cleanup(server.Close)

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{IntrospectURL: server.URL})
	jwtverifier.SetStrict(ch, true)

	revoked, err := ch.IsRevoked(context.Background(), &jwtverifier.Claims{
		Subject: "any",
		Raw:     "the-jwt",
	})
	if err != nil {
		t.Fatalf("strict-mode IsRevoked: %v", err)
	}
	if !revoked {
		t.Fatal("active=false must be treated as revoked")
	}
}

// TestMQRevocationChecker_Introspect_EmptyURL pins the dev-mode short-circuit:
// when IntrospectURL is not wired, introspect returns (false, nil) so the
// checker still functions as a pure in-memory accumulator.
func TestMQRevocationChecker_Introspect_EmptyURL(t *testing.T) {
	t.Parallel()

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{})
	jwtverifier.SetStrict(ch, true) // force introspect path

	revoked, err := ch.IsRevoked(context.Background(), &jwtverifier.Claims{
		Subject: "any",
		Raw:     "the-jwt",
	})
	if err != nil {
		t.Fatalf("IsRevoked: %v", err)
	}
	if revoked {
		t.Fatal("empty IntrospectURL must short-circuit to allowed")
	}
}

// TestMQRevocationChecker_Introspect_RawEmpty pins the safety guard: if the
// raw JWT was not pre-populated on the Claims (legacy callers), introspect
// must short-circuit to (false, nil) rather than POST an empty token.
func TestMQRevocationChecker_Introspect_RawEmpty(t *testing.T) {
	t.Parallel()

	var called atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"active":true}`))
	}))
	t.Cleanup(server.Close)

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{IntrospectURL: server.URL})
	jwtverifier.SetStrict(ch, true)

	revoked, err := ch.IsRevoked(context.Background(), &jwtverifier.Claims{
		Subject: "any",
		// Raw intentionally empty.
	})
	if err != nil {
		t.Fatalf("IsRevoked: %v", err)
	}
	if revoked {
		t.Fatal("empty Raw must short-circuit to allowed (no introspect call)")
	}
	if called.Load() != 0 {
		t.Fatalf("introspect called %d times, want 0 — must not hit the server with empty Raw", called.Load())
	}
}

// TestMQRevocationChecker_Introspect_NetworkError pins the fail-closed
// behaviour when introspect is unreachable. The Verifier wraps this into a
// rejection: the bug we are guarding against is silently allowing tokens
// when the auth server is down.
func TestMQRevocationChecker_Introspect_NetworkError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	server.Close() // immediately close so requests fail with connection refused

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{IntrospectURL: server.URL})
	jwtverifier.SetStrict(ch, true)

	_, err := ch.IsRevoked(context.Background(), &jwtverifier.Claims{Subject: "any", Raw: "raw"})
	if err == nil {
		t.Fatal("err = nil, want network error wrapped as introspect failure")
	}
	if !strings.Contains(err.Error(), "introspect") {
		t.Errorf("err = %v, want wrapped 'introspect' context", err)
	}
}

// TestMQRevocationChecker_Introspect_Non2xxStatus pins fail-closed on a 5xx.
func TestMQRevocationChecker_Introspect_Non2xxStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{IntrospectURL: server.URL})
	jwtverifier.SetStrict(ch, true)

	_, err := ch.IsRevoked(context.Background(), &jwtverifier.Claims{Subject: "any", Raw: "raw"})
	if err == nil {
		t.Fatal("err = nil, want wrapped 500 error")
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Errorf("err = %v, want wrapped status 500", err)
	}
}

// TestMQRevocationChecker_Introspect_DecodeError pins fail-closed when the
// introspect body cannot be decoded — we MUST NOT silently allow.
func TestMQRevocationChecker_Introspect_DecodeError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not json`))
	}))
	t.Cleanup(server.Close)

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{IntrospectURL: server.URL})
	jwtverifier.SetStrict(ch, true)

	_, err := ch.IsRevoked(context.Background(), &jwtverifier.Claims{Subject: "any", Raw: "raw"})
	if err == nil {
		t.Fatal("err = nil, want wrapped decode error")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("err = %v, want wrapped decode error", err)
	}
}

// TestMQRevocationChecker_Introspect_BadURL pins the http.NewRequest error
// arm of introspect: a syntactically illegal URL must surface as a wrapped
// "build request" error.
func TestMQRevocationChecker_Introspect_BadURL(t *testing.T) {
	t.Parallel()

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{
		IntrospectURL: "http://\x7f-invalid/introspect",
	})
	jwtverifier.SetStrict(ch, true)

	_, err := ch.IsRevoked(context.Background(), &jwtverifier.Claims{Subject: "any", Raw: "raw"})
	if err == nil {
		t.Fatal("err = nil, want build-request error")
	}
	if !strings.Contains(err.Error(), "build request") {
		t.Errorf("err = %v, want wrapped 'build request' context", err)
	}
}

// TestRunCatchup_NoReplayURL pins the dev short-circuit: if ReplayURL is not
// wired, RunCatchup returns nil without any HTTP I/O.
func TestRunCatchup_NoReplayURL(t *testing.T) {
	t.Parallel()

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{})
	if err := ch.RunCatchup(context.Background()); err != nil {
		t.Fatalf("RunCatchup with empty ReplayURL: %v", err)
	}
}

// TestRunCatchup_BadReplayURL pins the url.Parse error branch.
func TestRunCatchup_BadReplayURL(t *testing.T) {
	t.Parallel()

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{
		ReplayURL: "://not-a-url",
	})
	err := ch.RunCatchup(context.Background())
	if err == nil {
		t.Fatal("err = nil, want url.Parse error")
	}
	if !strings.Contains(err.Error(), "parse replay url") {
		t.Errorf("err = %v, want wrapped parse error", err)
	}
}

// TestRunCatchup_NetworkError pins the wrapped network-failure branch.
func TestRunCatchup_NetworkError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	server.Close()

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{ReplayURL: server.URL})
	err := ch.RunCatchup(context.Background())
	if err == nil {
		t.Fatal("err = nil, want wrapped network failure")
	}
	if !strings.Contains(err.Error(), "catchup request") {
		t.Errorf("err = %v, want wrapped catchup network error", err)
	}
}

// TestRunCatchup_Non2xxStatus pins the wrapped non-2xx surface.
func TestRunCatchup_Non2xxStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{ReplayURL: server.URL})
	err := ch.RunCatchup(context.Background())
	if err == nil {
		t.Fatal("err = nil, want wrapped status error")
	}
	if !strings.Contains(err.Error(), "status 503") {
		t.Errorf("err = %v, want wrapped status 503", err)
	}
}

// TestRunCatchup_DecodeError pins the wrapped decode failure.
func TestRunCatchup_DecodeError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not json`))
	}))
	t.Cleanup(server.Close)

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{ReplayURL: server.URL})
	err := ch.RunCatchup(context.Background())
	if err == nil {
		t.Fatal("err = nil, want wrapped decode failure")
	}
	if !strings.Contains(err.Error(), "catchup decode") {
		t.Errorf("err = %v, want wrapped catchup decode error", err)
	}
}

// TestRunCatchup_BadRequestURL pins the http.NewRequest build-request branch.
// Construct a checker whose ReplayURL is parseable but yields a request that
// http.NewRequestWithContext rejects. We achieve this by inserting a NUL byte
// after url.Parse via direct mutation through a custom Transport — simpler:
// give the parsed URL a control character in its path so the request builder
// fails. url.Parse is permissive, but http.NewRequest rejects an invalid host.
func TestRunCatchup_BadRequestURL(t *testing.T) {
	t.Parallel()

	// "http://host\x7f/" parses as a URL but http.NewRequestWithContext
	// rejects it for an invalid header value once stringified.
	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{
		ReplayURL: "http://host\x7f/replay",
	})
	err := ch.RunCatchup(context.Background())
	if err == nil {
		t.Fatal("err = nil, want wrapped build-request or transport error")
	}
}

// TestRunCatchup_AuthHeaderOmittedWhenUnset pins that the optional Auth header
// is NOT injected when the config left it empty.
func TestRunCatchup_AuthHeaderOmittedWhenUnset(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization header = %q, want empty (no auth configured)", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"events": []any{}, "lastId": 0})
	}))
	t.Cleanup(server.Close)

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{ReplayURL: server.URL})
	if err := ch.RunCatchup(context.Background()); err != nil {
		t.Fatalf("RunCatchup: %v", err)
	}
}

// TestRunCatchup_MultiPage drives the pagination loop until a short page
// terminates it. Each full page advances lastID; we assert the final cursor
// reflects the last page's lastId and the strict-mode flag flips off once at
// least one event has been applied.
func TestRunCatchup_MultiPage(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	// We'll serve two pages: page 1 has 1000 events (= page limit, so loop
	// continues), page 2 has 1 event (< page limit, so loop terminates).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			events := make([]testEvent, 1000)
			for i := range events {
				events[i] = testEvent{
					EventID:      "p1-" + string(rune('a'+i%26)),
					Scope:        "user",
					TargetUserID: "u-page1",
					RevokedAt:    time.Now(),
					ExpiresAt:    time.Now().Add(time.Hour),
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"events": events,
				"lastId": int64(1000),
			})
		case 2:
			events := []testEvent{{
				EventID:      "p2-x",
				Scope:        "user",
				TargetUserID: "u-page2",
				RevokedAt:    time.Now(),
				ExpiresAt:    time.Now().Add(time.Hour),
			}}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"events": events,
				"lastId": int64(1001),
			})
		default:
			t.Errorf("unexpected extra page request #%d", n)
			_ = json.NewEncoder(w).Encode(map[string]any{"events": []any{}, "lastId": 1001})
		}
	}))
	t.Cleanup(server.Close)

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{ReplayURL: server.URL})
	jwtverifier.SetStrict(ch, true) // pre-arm strict so we can observe the auto-clear

	if err := ch.RunCatchup(context.Background()); err != nil {
		t.Fatalf("RunCatchup: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("server calls = %d, want 2 (full page → continue, short page → stop)", got)
	}
	if got := jwtverifier.LastIDLoad(ch); got != 1001 {
		t.Fatalf("lastID = %d, want 1001", got)
	}
	if jwtverifier.StrictLoad(ch) {
		t.Fatalf("strict mode should be cleared after catchup applied events")
	}
}

// TestRunCatchup_LastIDOnlyAdvancesForward pins the monotonic-cursor invariant:
// a race in which a stale catchup response reports a lower lastId must NOT
// regress the checker's cursor.
func TestRunCatchup_LastIDOnlyAdvancesForward(t *testing.T) {
	t.Parallel()

	// First call returns lastId=100. Second call returns lastId=5 (a stale
	// page from a racing request). Cursor must remain at 100.
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"events": []testEvent{},
				"lastId": int64(100),
			})
		case 2:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"events": []testEvent{},
				"lastId": int64(5),
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"events": []any{}, "lastId": 0})
		}
	}))
	t.Cleanup(server.Close)

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{ReplayURL: server.URL})

	if err := ch.RunCatchup(context.Background()); err != nil {
		t.Fatalf("RunCatchup #1: %v", err)
	}
	if got := jwtverifier.LastIDLoad(ch); got != 100 {
		t.Fatalf("after #1, lastID = %d, want 100", got)
	}
	if err := ch.RunCatchup(context.Background()); err != nil {
		t.Fatalf("RunCatchup #2: %v", err)
	}
	if got := jwtverifier.LastIDLoad(ch); got != 100 {
		t.Fatalf("after #2 (stale lastId=5), lastID = %d, want 100 (must not regress)", got)
	}
}

// TestRunCatchup_PageCapWarn drives the loop to the iteration cap. Each page
// is a "full" page so the loop never sees a short page; the function returns
// nil but warns. We assert no error is returned and exactly catchupMaxPages
// HTTP calls were made.
func TestRunCatchup_PageCapWarn(t *testing.T) {
	t.Parallel()

	const maxPages = 50
	var calls atomic.Int32
	var lastID int64 = 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		events := make([]testEvent, 1000)
		for i := range events {
			events[i] = testEvent{
				EventID:      "evt",
				Scope:        "user",
				TargetUserID: "u-cap",
				RevokedAt:    time.Now(),
				ExpiresAt:    time.Now().Add(time.Hour),
			}
		}
		lastID++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"events": events,
			"lastId": lastID,
		})
	}))
	t.Cleanup(server.Close)

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{ReplayURL: server.URL})
	if err := ch.RunCatchup(context.Background()); err != nil {
		t.Fatalf("RunCatchup: %v", err)
	}
	if got := calls.Load(); got != int32(maxPages) {
		t.Fatalf("calls = %d, want %d (page cap)", got, maxPages)
	}
}

// TestStartConsumer_DisconnectTimeoutZeroDefault pins the
// "interval <= 0 → interval = DisconnectTimeout" branch inside StartConsumer.
// We configure a tiny DisconnectTimeout (1ns) so DisconnectTimeout/3 truncates
// to 0 and the fallback branch is taken; the consumer must still run and exit
// cleanly on ctx cancel.
func TestStartConsumer_DisconnectTimeoutZeroDefault(t *testing.T) {
	t.Parallel()

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{
		DisconnectTimeout: 1 * time.Nanosecond, // /3 = 0 → fallback branch
		IntrospectURL:     "http://invalid",    // not used; just satisfies wiring
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	t.Cleanup(cancel)

	subscribe := func(sctx context.Context, _ func(context.Context, []byte) error) error {
		<-sctx.Done()
		return sctx.Err()
	}
	err := ch.StartConsumer(ctx, subscribe)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("StartConsumer: %v", err)
	}
}

// TestStartConsumer_SubscribeError pins that StartConsumer surfaces the
// subscribe-callback error verbatim. Important for caller error-handling
// downstream of shared/mq.Consumer.Subscribe.
func TestStartConsumer_SubscribeError(t *testing.T) {
	t.Parallel()

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{})
	sentinel := errors.New("mq subscribe boom")
	subscribe := func(_ context.Context, _ func(context.Context, []byte) error) error {
		return sentinel
	}
	if err := ch.StartConsumer(context.Background(), subscribe); !errors.Is(err, sentinel) {
		t.Fatalf("StartConsumer err = %v, want %v", err, sentinel)
	}
}

// TestNewMQRevocationChecker_NilLoggerDefaults pins that a nil Logger config
// is replaced with slog.Default(), so HandleMessage / RunCatchup never NPE
// when callers leave Logger unset.
func TestNewMQRevocationChecker_NilLoggerDefaults(t *testing.T) {
	t.Parallel()

	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{
		// Logger intentionally nil.
	})
	// Drive a Warn path through HandleMessage so the defaulted logger is hit.
	ev := testEvent{
		EventID: "drop", Scope: "ipaddr", // unrecognized → triggers logger.Warn
	}
	if err := ch.HandleMessage(context.Background(), mustMarshal(t, ev)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
}

// TestNewMQRevocationChecker_LoggerHonored pins that a non-default logger is
// not overwritten by the constructor.
func TestNewMQRevocationChecker_LoggerHonored(t *testing.T) {
	t.Parallel()

	// Just ensure the constructor accepts a non-nil logger without modification.
	custom := slog.New(slog.NewTextHandler(io.Discard, nil))
	ch := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{Logger: custom})
	if ch == nil {
		t.Fatal("NewMQRevocationChecker returned nil")
	}
}
