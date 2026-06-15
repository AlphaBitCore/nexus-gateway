package assistant

import (
	"fmt"
	"sort"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
)

// memMemory is the pool-less fallback MemoryStore (no persistence). It is
// constructed ONE PER SESSION/CALLER, so isolation is structural (the instance
// only ever holds one user's facts) — honoring the kernel's isolation contract.
// A DB-wired deployment uses the userId-scoped DB-backed impl instead.
type memMemory struct {
	facts map[string]agent.MemoryFact
}

func newMemMemory() *memMemory { return &memMemory{facts: map[string]agent.MemoryFact{}} }

func (m *memMemory) Index() (string, error) {
	if len(m.facts) == 0 {
		return "", nil
	}
	names := make([]string, 0, len(m.facts))
	for n := range m.facts {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic so the per-turn prompt prefix stays stable
	var b strings.Builder
	for _, n := range names {
		f := m.facts[n]
		desc := f.Description
		if desc == "" {
			desc = f.Body
		}
		fmt.Fprintf(&b, "- %s [%s] — %s\n", n, f.Type, desc)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func (m *memMemory) Recall(name string) (agent.MemoryFact, bool, error) {
	f, ok := m.facts[name]
	return f, ok, nil
}

func (m *memMemory) Remember(f agent.MemoryFact) error { m.facts[f.Name] = f; return nil }

func (m *memMemory) Update(name, body string) error {
	f, ok := m.facts[name]
	if !ok {
		return fmt.Errorf("no memory named %q to update", name)
	}
	f.Body = body
	m.facts[name] = f
	return nil
}

func (m *memMemory) Forget(name string) (bool, error) {
	_, ok := m.facts[name]
	delete(m.facts, name)
	return ok, nil
}

var _ agent.MemoryStore = (*memMemory)(nil)

// memStore is the pool-less fallback SessionStore (no persistence). One instance
// per caller; a DB-wired deployment uses DB(metadata)+spill(transcript) instead.
type memStore struct {
	sessions map[string]*agent.Session
}

func newMemStore() *memStore { return &memStore{sessions: map[string]*agent.Session{}} }

func (s *memStore) Save(sess *agent.Session) error { s.sessions[sess.ID] = sess; return nil }

func (s *memStore) Load(id string) (*agent.Session, error) {
	sess, ok := s.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %s not found", id)
	}
	return sess, nil
}

func (s *memStore) List() ([]agent.SessionMeta, error) {
	out := make([]agent.SessionMeta, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, agent.SessionMeta{ID: sess.ID, Env: sess.Env})
	}
	return out, nil
}

var _ agent.SessionStore = (*memStore)(nil)
