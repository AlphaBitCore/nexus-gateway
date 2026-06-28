package store

import (
	"sync"
	"time"
)

// AuthCodeEntry is the short-lived record binding an authorization code to
// the originating authorize request. Entries are single-use: any successful
// Get deletes the entry.
//
// Nonce, Email and AMR are carried through to the token endpoint so ID-token
// construction can include the RFC 7519 claims without a second
// DB roundtrip. Nonce mirrors the OIDC authorize-request parameter; Email and
// AMR are captured from the IdP authentication result.
type AuthCodeEntry struct {
	ClientID      string
	UserID        string
	RedirectURI   string
	PKCEChallenge string // S256 only
	Scope         string
	SessionID     string
	IdPID         string
	DeviceID      string // may be empty for non-agent flows
	Nonce         string
	Email         string
	AMR           []string
	ExpiresAt     time.Time
}

// AuthCodeStore holds authorization codes in process memory. Entries do not
// survive a restart; a restart invalidates any in-flight OAuth flows, which is
// acceptable given the short TTL (caller-supplied per entry via ExpiresAt).
// There is deliberately no Delete method: Get consumes the entry.
type AuthCodeStore struct {
	mu   sync.Mutex
	data map[string]AuthCodeEntry

	done     chan struct{}
	closeOne sync.Once
	ticker   *time.Ticker
	wg       sync.WaitGroup
}

// NewAuthCodeStore returns a store whose janitor sweeps expired entries on a
// ticker keyed to defaultTTL. Callers must invoke Close to stop the janitor.
func NewAuthCodeStore(defaultTTL time.Duration) *AuthCodeStore {
	s := &AuthCodeStore{
		data:   make(map[string]AuthCodeEntry),
		done:   make(chan struct{}),
		ticker: time.NewTicker(janitorInterval(defaultTTL)),
	}
	s.wg.Add(1)
	go s.run()
	return s
}

// Put stores entry under code, overwriting any prior value.
func (s *AuthCodeStore) Put(code string, entry AuthCodeEntry) {
	s.mu.Lock()
	s.data[code] = entry
	s.mu.Unlock()
}

// Get returns the entry for code and deletes it. Expired entries are removed
// and reported as missing.
func (s *AuthCodeStore) Get(code string) (AuthCodeEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data[code]
	if !ok {
		return AuthCodeEntry{}, false
	}
	delete(s.data, code)
	if time.Now().After(e.ExpiresAt) {
		return AuthCodeEntry{}, false
	}
	return e, true
}

// Close stops the janitor. Safe to call more than once.
func (s *AuthCodeStore) Close() {
	s.closeOne.Do(func() {
		close(s.done)
		s.ticker.Stop()
	})
	s.wg.Wait()
}

func (s *AuthCodeStore) run() {
	defer s.wg.Done()
	for {
		select {
		case <-s.done:
			return
		case <-s.ticker.C:
			s.sweep()
		}
	}
}

func (s *AuthCodeStore) sweep() {
	now := time.Now()
	s.mu.Lock()
	for k, v := range s.data {
		if now.After(v.ExpiresAt) {
			delete(s.data, k)
		}
	}
	s.mu.Unlock()
}

// janitorInterval clamps the sweep cadence to max(1s, ttl/5) so short-TTL tests
// do not wait forever while keeping production sweeps cheap.
func janitorInterval(ttl time.Duration) time.Duration {
	step := ttl / 5
	if step < time.Second {
		return time.Second
	}
	return step
}
