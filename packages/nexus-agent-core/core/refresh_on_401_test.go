package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// refresherStub is a TokenSource that also implements credentialRefresher. Credential
// hands out `cur`; RefreshCredential swaps cur→next (recording the call) or returns
// refreshErr. Used to drive the client's reactive refresh-on-401 path.
type refresherStub struct {
	cur, next  string
	refreshErr error
	refreshN   int
}

func (s *refresherStub) Credential(context.Context) (string, string, error) {
	return "Authorization", s.cur, nil
}

func (s *refresherStub) RefreshCredential(_ context.Context, _ string) (string, string, error) {
	s.refreshN++
	if s.refreshErr != nil {
		return "", "", s.refreshErr
	}
	s.cur = s.next
	return "Authorization", s.next, nil
}

func TestClient_RefreshOn401_RetriesWithNewToken(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") == "new" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"token rejected"}}`))
	}))
	defer srv.Close()

	src := &refresherStub{cur: "old", next: "new"}
	c := NewClient(Env{Name: "local", CPBaseURL: srv.URL}, src, srv.Client())
	_, status, err := c.AdminRequest(context.Background(), http.MethodGet, "/x", nil, nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("after refresh-on-401: status=%d err=%v, want 200/nil", status, err)
	}
	if calls != 2 {
		t.Fatalf("server calls=%d, want 2 (initial 401 + one retry)", calls)
	}
	if src.refreshN != 1 {
		t.Fatalf("RefreshCredential calls=%d, want exactly 1", src.refreshN)
	}
}

func TestClient_RefreshOn401_NoRetryWhenSourceCannotRefresh(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
	}))
	defer srv.Close()

	// fixedTokenSource does NOT implement credentialRefresher → no retry.
	c := NewClient(Env{Name: "local", CPBaseURL: srv.URL},
		fixedTokenSource{header: "Authorization", value: "old"}, srv.Client())
	_, status, err := c.AdminRequest(context.Background(), http.MethodGet, "/x", nil, nil)
	if status != http.StatusUnauthorized || !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("non-refresher: status=%d err=%v, want 401/ErrUnauthorized", status, err)
	}
	if calls != 1 {
		t.Fatalf("server calls=%d, want 1 (no retry without a refresher)", calls)
	}
}

func TestClient_RefreshOn401_StillUnauthorizedAfterRetry(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++ // 401 regardless of token
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"still no"}}`))
	}))
	defer srv.Close()

	src := &refresherStub{cur: "old", next: "new"}
	c := NewClient(Env{Name: "local", CPBaseURL: srv.URL}, src, srv.Client())
	_, status, err := c.AdminRequest(context.Background(), http.MethodGet, "/x", nil, nil)
	if status != http.StatusUnauthorized || !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("retry-still-401: status=%d err=%v, want 401/ErrUnauthorized", status, err)
	}
	if calls != 2 {
		t.Fatalf("server calls=%d, want 2 (bounded to a single retry, no loop)", calls)
	}
}

func TestClient_RefreshOn401_NoRetryWhenRefreshUnchanged(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"no"}}`))
	}))
	defer srv.Close()

	// RefreshCredential returns the SAME value (e.g. nothing to rotate) → the client must
	// NOT retry, since the same token would just 401 again.
	src := &refresherStub{cur: "old", next: "old"}
	c := NewClient(Env{Name: "local", CPBaseURL: srv.URL}, src, srv.Client())
	_, status, _ := c.AdminRequest(context.Background(), http.MethodGet, "/x", nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("unchanged-cred: status=%d, want 401", status)
	}
	if calls != 1 {
		t.Fatalf("server calls=%d, want 1 (no retry when refreshed value is unchanged)", calls)
	}
}

func TestClient_RefreshOn401_NoRetryWhenRefreshErrors(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"no"}}`))
	}))
	defer srv.Close()

	src := &refresherStub{cur: "old", next: "new", refreshErr: &APIError{kind: ErrUnauthorized, Message: "login required"}}
	c := NewClient(Env{Name: "local", CPBaseURL: srv.URL}, src, srv.Client())
	_, status, _ := c.AdminRequest(context.Background(), http.MethodGet, "/x", nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("refresh-error: status=%d, want 401 (original)", status)
	}
	if calls != 1 {
		t.Fatalf("server calls=%d, want 1 (no retry when refresh fails)", calls)
	}
}

func TestJWTTokenSource_RefreshCredential_ForcesRefresh(t *testing.T) {
	now := time.Now()
	store := newMemStore()
	oldTok := makeTestJWT(t, now.Add(time.Hour)) // not near expiry — proactive path wouldn't refresh
	_ = store.Set("local", SecretAccessToken, oldTok)
	_ = store.Set("local", SecretRefreshToken, "r1")

	newTok := makeTestJWT(t, now.Add(2*time.Hour))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "r1" {
			t.Errorf("bad refresh form: %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: newTok, RefreshToken: "r2", ExpiresIn: 3600})
	}))
	defer srv.Close()

	src := newJWTSource(t, store, srv.URL, now)
	_, v, err := src.RefreshCredential(context.Background(), "Bearer "+oldTok)
	if err != nil || v != "Bearer "+newTok {
		t.Fatalf("force refresh: v=%q err=%v, want Bearer %s", v, err, newTok)
	}
	if got, _ := store.Get("local", SecretAccessToken); got != newTok {
		t.Fatalf("access token not updated: %q", got)
	}
	if got, _ := store.Get("local", SecretRefreshToken); got != "r2" {
		t.Fatalf("refresh token not rotated: %q", got)
	}
}

func TestJWTTokenSource_RefreshCredential_SingleFlight(t *testing.T) {
	now := time.Now()
	store := newMemStore()
	// The stored token already differs from the rejected one → a concurrent caller
	// refreshed first; RefreshCredential must return the current token WITHOUT a refresh.
	_ = store.Set("local", SecretAccessToken, "already-new")
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("refresh endpoint must NOT be hit when the stored token already changed")
	}))
	defer srv.Close()

	src := newJWTSource(t, store, srv.URL, now)
	_, v, err := src.RefreshCredential(context.Background(), "Bearer stale-old")
	if err != nil || v != "Bearer already-new" {
		t.Fatalf("single-flight: v=%q err=%v, want Bearer already-new", v, err)
	}
}

func TestJWTTokenSource_RefreshCredential_RefreshFails(t *testing.T) {
	now := time.Now()
	store := newMemStore()
	oldTok := makeTestJWT(t, now.Add(time.Hour))
	_ = store.Set("local", SecretAccessToken, oldTok)
	// no refresh token stored → refresh grant fails
	src := newJWTSource(t, store, "http://unused", now)
	if _, _, err := src.RefreshCredential(context.Background(), "Bearer "+oldTok); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("refresh-fail: want ErrUnauthorized, got %v", err)
	}
}
