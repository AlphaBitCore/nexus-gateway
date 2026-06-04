package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// TestErrSecretNotFoundIsStable guards that memStore.Get reports the shared
// sentinel — the kernel's SecretStore contract every backend must honor.
func TestErrSecretNotFoundIsStable(t *testing.T) {
	_, err := newMemStore().Get("local", SecretAccessToken)
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("want ErrSecretNotFound, got %v", err)
	}
}

// memSecretStore is an in-memory SecretStore for tests. Keyed by "<env>:<key>".
// setErr, when non-nil, makes Set fail (to exercise persistence-error paths).
type memSecretStore struct {
	m      map[string]string
	setErr error
}

func newMemStore() *memSecretStore { return &memSecretStore{m: map[string]string{}} }

func (s *memSecretStore) Get(env, key string) (string, error) {
	v, ok := s.m[env+":"+key]
	if !ok {
		return "", ErrSecretNotFound
	}
	return v, nil
}

func (s *memSecretStore) Set(env, key, val string) error {
	if s.setErr != nil {
		return s.setErr
	}
	s.m[env+":"+key] = val
	return nil
}

func (s *memSecretStore) Delete(env, key string) error {
	delete(s.m, env+":"+key)
	return nil
}

// fixedTokenSource returns a constant credential (or error) for client tests.
type fixedTokenSource struct {
	header, value string
	err           error
}

func (f fixedTokenSource) Credential(context.Context) (string, string, error) {
	return f.header, f.value, f.err
}

// makeTestJWT builds an unsigned JWT whose payload carries the given exp. Only
// the payload segment must be valid base64url JSON; jwtExpiry never verifies.
func makeTestJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payloadJSON, err := json.Marshal(map[string]int64{"exp": exp.Unix()})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	return header + "." + payload + ".sig"
}
