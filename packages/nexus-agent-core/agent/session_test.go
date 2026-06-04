package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionStoreSaveLoadList(t *testing.T) {
	st := openStoreAt(t.TempDir())

	// List on a not-yet-created dir is empty, not an error.
	if metas, err := st.List(); err != nil || metas != nil {
		t.Fatalf("empty store lists nothing, got %v err %v", metas, err)
	}

	s1 := NewSession("prod")
	s1.Messages = []Message{TextMessage(RoleUser, "why is cost high?"), TextMessage(RoleAssistant, "anthropic spike")}
	s1.NavTrail = []string{"mission-control", "cost"}
	if err := st.Save(s1); err != nil {
		t.Fatal(err)
	}

	got, err := st.Load(s1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 || got.Messages[0].Text() != "why is cost high?" {
		t.Fatalf("loaded transcript mismatch: %+v", got.Messages)
	}
	if len(got.NavTrail) != 2 || got.NavTrail[1] != "cost" {
		t.Fatalf("nav trail must round-trip, got %v", got.NavTrail)
	}

	time.Sleep(2 * time.Millisecond)
	s2 := NewSession("prod")
	s2.Messages = []Message{TextMessage(RoleUser, "list out-of-sync nodes")}
	if err := st.Save(s2); err != nil {
		t.Fatal(err)
	}
	metas, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 {
		t.Fatalf("List should see both sessions, got %d", len(metas))
	}
	if metas[0].ID != s2.ID {
		t.Fatalf("List is newest-first, got %s first", metas[0].ID)
	}
	if metas[0].Title == "" {
		t.Fatal("List meta should derive a title from the first user message")
	}
}

func TestSessionStoreIOErrorsSurface(t *testing.T) {
	// Point the store dir at an existing file: mkdir + readdir fail, and the
	// failure must surface.
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := openStoreAt(f)
	if err := st.Save(NewSession("local")); err == nil {
		t.Fatal("Save under a file-path dir must error")
	}
	if _, err := st.List(); err == nil {
		t.Fatal("List under a file-path dir must error")
	}
}

func TestOpenStoreResolvesPerEnvDir(t *testing.T) {
	st, err := OpenStore("kerneltest")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.dir, filepath.Join("nexus", "sessions", "kerneltest")) {
		t.Fatalf("OpenStore must resolve a per-env dir, got %q", st.dir)
	}
}

func TestSessionStoreErrorsAndTitles(t *testing.T) {
	st := openStoreAt(t.TempDir())

	// Save with no id is an error.
	if err := st.Save(&Session{}); err == nil {
		t.Fatal("a session without an id must not save")
	}
	// Load a missing session errors.
	if _, err := st.Load("nope"); err == nil {
		t.Fatal("loading a missing session must error")
	}

	// Long first message → truncated title with an ellipsis.
	long := NewSession("local")
	long.Messages = []Message{TextMessage(RoleUser, strings.Repeat("x", 80))}
	if title := sessionTitle(long); !strings.HasSuffix(title, "…") || len([]rune(title)) > 62 {
		t.Fatalf("long title must be truncated with an ellipsis, got %q", title)
	}
	// No user message → fallback title.
	empty := NewSession("local")
	empty.Messages = []Message{{Role: RoleAssistant, Blocks: []Block{{Type: BlockText, Text: "hi"}}}}
	if sessionTitle(empty) != "(empty session)" {
		t.Fatalf("no user message → fallback title, got %q", sessionTitle(empty))
	}

	// A corrupt .json file is skipped by List rather than failing it.
	if err := os.MkdirAll(st.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(st.dir, "bad.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok := NewSession("local")
	ok.Messages = []Message{TextMessage(RoleUser, "good one")}
	if err := st.Save(ok); err != nil {
		t.Fatal(err)
	}
	metas, err := st.List()
	if err != nil {
		t.Fatalf("a corrupt file must not fail List, err=%v", err)
	}
	if len(metas) != 1 || metas[0].ID != ok.ID {
		t.Fatalf("List should skip the corrupt file and return the good one, got %+v", metas)
	}
}

func TestOpenStoreAtUsesGivenDir(t *testing.T) {
	dir := t.TempDir()
	st := OpenStoreAt(dir)
	sess := NewSession("local")
	if err := st.Save(sess); err != nil {
		t.Fatal(err)
	}
	metas, err := st.List()
	if err != nil || len(metas) != 1 || metas[0].ID != sess.ID {
		t.Fatalf("OpenStoreAt must open a working store at the dir, got metas=%+v err=%v", metas, err)
	}
}
