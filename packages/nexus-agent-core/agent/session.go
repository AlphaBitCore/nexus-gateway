package agent

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/goccy/go-json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Session is a persisted agent conversation: the full transcript plus the
// view/navigation trail and timing. Auto-saved each turn; can be listed,
// switched, and resumed (distinct from in-window compaction and durable memory).
type Session struct {
	ID       string    `json:"id"`
	Env      string    `json:"env"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	Messages []Message `json:"messages"`
	NavTrail []string  `json:"nav_trail,omitempty"`
}

// NewSession starts an empty session with a random id and timestamps.
func NewSession(env string) *Session {
	now := time.Now().UTC()
	return &Session{ID: newSessionID(), Env: env, Created: now, Updated: now}
}

func newSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// SessionMeta is the listing summary.
type SessionMeta struct {
	ID      string
	Env     string
	Updated time.Time
	Title   string
	// Messages is the persisted transcript length — the "12 msgs" badge a
	// session picker shows next to the title.
	Messages int
}

// SessionStore is the kernel's session-persistence seam. The CLI binds a
// file-backed impl; the web binds a DB(metadata)+S3(transcript) impl constructed
// bound to a userId.
type SessionStore interface {
	Save(sess *Session) error
	Load(id string) (*Session, error)
	List() ([]SessionMeta, error)
}

// Store persists sessions under a per-env directory.
type Store struct {
	dir string
}

// OpenStore resolves ~/.config/nexus/sessions/<env>/ for the given env.
func OpenStore(env string) (*Store, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("resolve user config dir: %w", err)
	}
	return openStoreAt(filepath.Join(d, "nexus", "sessions", env)), nil
}

// openStoreAt is the path-injection seam for tests.
func openStoreAt(dir string) *Store { return &Store{dir: dir} }

// OpenStoreAt opens a session store at an explicit directory. Layer 2 uses it to
// wire the per-env session dir resolved by the toolkit's own config layer.
func OpenStoreAt(dir string) *Store { return openStoreAt(dir) }

// Save writes the session as <id>.json (atomic via temp + rename), stamping Updated.
func (s *Store) Save(sess *Session) error {
	if sess.ID == "" {
		return fmt.Errorf("session has no id")
	}
	sess.Updated = time.Now().UTC()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}
	final := filepath.Join(s.dir, sess.ID+".json")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write session: %w", err)
	}
	return os.Rename(tmp, final)
}

// Load reads a session by id.
func (s *Store) Load(id string) (*Session, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, id+".json"))
	if err != nil {
		return nil, fmt.Errorf("load session %s: %w", id, err)
	}
	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, fmt.Errorf("parse session %s: %w", id, err)
	}
	return &sess, nil
}

// Delete removes a persisted session by id. Deleting a session that does not
// exist is an error — the caller offered it from a listing, so a miss means the
// listing is stale and should be surfaced, not swallowed.
func (s *Store) Delete(id string) error {
	if id == "" {
		return fmt.Errorf("session has no id")
	}
	if err := os.Remove(filepath.Join(s.dir, id+".json")); err != nil {
		return fmt.Errorf("delete session %s: %w", id, err)
	}
	return nil
}

// List returns session metadata newest-first (by Updated).
func (s *Store) List() ([]SessionMeta, error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read session dir: %w", err)
	}
	var metas []SessionMeta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		sess, err := s.Load(id)
		if err != nil {
			continue // skip a corrupt file rather than failing the whole list
		}
		metas = append(metas, SessionMeta{ID: sess.ID, Env: sess.Env, Updated: sess.Updated, Title: sessionTitle(sess), Messages: len(sess.Messages)})
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Updated.After(metas[j].Updated) })
	return metas, nil
}

// sessionTitle derives a short title from the first user message.
func sessionTitle(sess *Session) string {
	for _, m := range sess.Messages {
		if m.Role == RoleUser {
			t := strings.TrimSpace(m.Text())
			if len(t) > 60 {
				t = CutText(t, 60) + "…"
			}
			if t != "" {
				return t
			}
		}
	}
	return "(empty session)"
}
