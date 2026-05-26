package store

import (
	"sync"
	"time"
)

// bindingTTL is the default lifetime of a device-binding handle.
const bindingTTL = 5 * time.Minute

// BindingEntry is the short-lived device-binding handle surfaced by
// /oauth/device-binding. Entries are NOT consumed on Get — they are referenced
// by state later during /oauth/authorize.
type BindingEntry struct {
	DeviceID      string
	State         string
	CodeChallenge string // S256
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

// BindingStore holds device-binding handles in process memory. Entries do not
// survive a restart; callers must treat a missing binding as a fresh
// device-binding request.
type BindingStore struct {
	mu   sync.Mutex
	data map[string]BindingEntry

	done     chan struct{}
	closeOne sync.Once
	ticker   *time.Ticker
	wg       sync.WaitGroup
}

// NewBindingStore returns a store whose janitor sweeps expired entries on a
// cadence tied to bindingTTL. Callers must invoke Close to stop the janitor.
func NewBindingStore() *BindingStore {
	s := &BindingStore{
		data:   make(map[string]BindingEntry),
		done:   make(chan struct{}),
		ticker: time.NewTicker(janitorInterval(bindingTTL)),
	}
	s.wg.Add(1)
	go s.run()
	return s
}

// Put stores entry under state, overwriting any prior value.
func (s *BindingStore) Put(state string, entry BindingEntry) {
	s.mu.Lock()
	s.data[state] = entry
	s.mu.Unlock()
}

// Get returns the binding for state without deleting it. Expired bindings are
// removed and reported as missing.
func (s *BindingStore) Get(state string) (BindingEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data[state]
	if !ok {
		return BindingEntry{}, false
	}
	if time.Now().After(e.ExpiresAt) {
		delete(s.data, state)
		return BindingEntry{}, false
	}
	return e, true
}

// Delete removes the binding for state. No-op when state is unknown.
func (s *BindingStore) Delete(state string) {
	s.mu.Lock()
	delete(s.data, state)
	s.mu.Unlock()
}

// Close stops the janitor. Safe to call more than once.
func (s *BindingStore) Close() {
	s.closeOne.Do(func() {
		close(s.done)
		s.ticker.Stop()
	})
	s.wg.Wait()
}

func (s *BindingStore) run() {
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

func (s *BindingStore) sweep() {
	now := time.Now()
	s.mu.Lock()
	for k, v := range s.data {
		if now.After(v.ExpiresAt) {
			delete(s.data, k)
		}
	}
	s.mu.Unlock()
}
