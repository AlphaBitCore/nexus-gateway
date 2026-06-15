package agent

import "testing"

// These tests prove the R1 seams (MemoryStore / SessionStore) are genuinely
// substitutable AND that per-user isolation can be enforced entirely inside an
// implementation — the design the web face relies on (a per-caller instance whose
// backend filters by the bound userId). They are the AC-3 evidence: a store bound
// to user A can never read user B's data.

// --- MemoryStore seam ---

// userScopedMemory models the web MemoryStore: an instance bound to a userId over
// a shared backend, so isolation is structural (keyed by userId), not by trust.
type userScopedMemory struct {
	userID  string
	backing map[string]map[string]MemoryFact // userID -> name -> fact
}

func newUserScopedMemory(userID string, shared map[string]map[string]MemoryFact) *userScopedMemory {
	if shared[userID] == nil {
		shared[userID] = map[string]MemoryFact{}
	}
	return &userScopedMemory{userID: userID, backing: shared}
}

func (m *userScopedMemory) Index() (string, error) { return "", nil }
func (m *userScopedMemory) Recall(name string) (MemoryFact, bool, error) {
	f, ok := m.backing[m.userID][name]
	return f, ok, nil
}
func (m *userScopedMemory) Remember(f MemoryFact) error { m.backing[m.userID][f.Name] = f; return nil }
func (m *userScopedMemory) Update(name, body string) error {
	f := m.backing[m.userID][name]
	f.Body = body
	m.backing[m.userID][name] = f
	return nil
}
func (m *userScopedMemory) Forget(name string) (bool, error) {
	_, ok := m.backing[m.userID][name]
	delete(m.backing[m.userID], name)
	return ok, nil
}

var _ MemoryStore = (*userScopedMemory)(nil)

func TestMemoryStoreSeamIsolatesUsers(t *testing.T) {
	shared := map[string]map[string]MemoryFact{}
	alice := newUserScopedMemory("alice", shared)
	bob := newUserScopedMemory("bob", shared)

	if err := alice.Remember(MemoryFact{Name: "secret", Type: MemPreference, Body: "alice-only"}); err != nil {
		t.Fatal(err)
	}
	// The core invariant: Bob's instance must NEVER surface Alice's fact.
	if _, ok, _ := bob.Recall("secret"); ok {
		t.Fatal("isolation breach: user B's MemoryStore recalled user A's fact")
	}
	if f, ok, _ := alice.Recall("secret"); !ok || f.Body != "alice-only" {
		t.Fatal("user A must recall her own fact")
	}
	// The seam plugs into the kernel: New accepts a MemoryStore impl and wires the
	// recall/remember/... tools onto it.
	reg := NewRegistry()
	ag := New(Config{Memory: bob, Store: newUserScopedStore("bob", map[string]map[string]*Session{}),
		Registry: reg, Gate: NewGate(NewCommandClassifier(), nil, false),
		Session: NewSession("web")})
	if ag == nil {
		t.Fatal("New must accept a userScopedMemory as the MemoryStore seam")
	}
	if _, ok := reg.Get("recall"); !ok {
		t.Fatal("New must register the memory tools onto the injected MemoryStore")
	}
}

// --- SessionStore seam ---

type userScopedStore struct {
	userID  string
	backing map[string]map[string]*Session // userID -> id -> session
}

func newUserScopedStore(userID string, shared map[string]map[string]*Session) *userScopedStore {
	if shared[userID] == nil {
		shared[userID] = map[string]*Session{}
	}
	return &userScopedStore{userID: userID, backing: shared}
}

func (s *userScopedStore) Save(sess *Session) error { s.backing[s.userID][sess.ID] = sess; return nil }
func (s *userScopedStore) Load(id string) (*Session, error) {
	sess, ok := s.backing[s.userID][id]
	if !ok {
		return nil, errNotFound
	}
	return sess, nil
}
func (s *userScopedStore) List() ([]SessionMeta, error) {
	out := make([]SessionMeta, 0, len(s.backing[s.userID]))
	for _, sess := range s.backing[s.userID] {
		out = append(out, SessionMeta{ID: sess.ID, Env: sess.Env})
	}
	return out, nil
}

var _ SessionStore = (*userScopedStore)(nil)

var errNotFound = errSentinel("session not found")

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

func TestSessionStoreSeamIsolatesUsers(t *testing.T) {
	shared := map[string]map[string]*Session{}
	alice := newUserScopedStore("alice", shared)
	bob := newUserScopedStore("bob", shared)

	asess := NewSession("web")
	if err := alice.Save(asess); err != nil {
		t.Fatal(err)
	}
	// Bob must not be able to Load Alice's session by id, nor see it in List.
	if _, err := bob.Load(asess.ID); err == nil {
		t.Fatal("isolation breach: user B loaded user A's session by id")
	}
	if metas, _ := bob.List(); len(metas) != 0 {
		t.Fatalf("isolation breach: user B's session list leaked %d of user A's sessions", len(metas))
	}
	if metas, _ := alice.List(); len(metas) != 1 {
		t.Fatalf("user A must see her own session, got %d", len(metas))
	}
}
