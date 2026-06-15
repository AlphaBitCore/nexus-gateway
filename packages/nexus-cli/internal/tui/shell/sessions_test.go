package shell

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
)

// fakeBrowser is a scriptable SessionBrowser for the failure-mode tests; the
// happy paths use the REAL agent store so list/delete durability is exercised
// against actual files.
type fakeBrowser struct {
	metas   []agent.SessionMeta
	loaded  map[string]*agent.Session
	listErr error
	loadErr error
	delErr  error
	deleted []string
}

func (f *fakeBrowser) List() ([]agent.SessionMeta, error) { return f.metas, f.listErr }
func (f *fakeBrowser) Load(id string) (*agent.Session, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	s, ok := f.loaded[id]
	if !ok {
		return nil, errors.New("no such session")
	}
	return s, nil
}
func (f *fakeBrowser) Delete(id string) error {
	if f.delErr != nil {
		return f.delErr
	}
	f.deleted = append(f.deleted, id)
	return nil
}

// seedStore writes n titled sessions (oldest first) into a real on-disk store
// and returns it. Saves are spaced so Updated ordering is deterministic.
func seedStore(t *testing.T, dir string, titles ...string) *agent.Store {
	t.Helper()
	st := agent.OpenStoreAt(dir)
	for _, title := range titles {
		s := agent.NewSession("local")
		s.Messages = []agent.Message{
			agent.TextMessage(agent.RoleUser, title),
			agent.TextMessage(agent.RoleAssistant, "answer to "+title),
		}
		if err := st.Save(s); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	return st
}

// openPickerViaTypedCommand types /sessions into the chat prompt and drives the
// whole flow through the ROOT Update: enter → the conversation emits
// openSessionsMsg → the root opens the picker.
func openPickerViaTypedCommand(t *testing.T, m Model) Model {
	t.Helper()
	m.conv.input.SetValue("/sessions")
	m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("/sessions must emit the open-picker command")
	}
	msg := cmd()
	if _, ok := msg.(openSessionsMsg); !ok {
		t.Fatalf("/sessions must request the picker, got %#v", msg)
	}
	m, _ = updateModel(m, msg)
	return m
}

func TestModelSessionsPickerListsNewestFirstWithTitles(t *testing.T) {
	st := seedStore(t, t.TempDir(), "first question", "second question")
	m, _ := newTestModel(testSessionLocal(), noTurn)
	m.openSessions = func() (SessionBrowser, error) { return st, nil }
	m.width, m.height = 120, 40

	m = openPickerViaTypedCommand(t, m)
	if !m.sessionsOpen {
		t.Fatal("/sessions must open the picker")
	}
	// The picker owns the footer (like the slash palette) and lists newest-first:
	// title + relative time + message count.
	out := stripSGR(m.footerBar(120))
	first := strings.Index(out, "second question")
	second := strings.Index(out, "first question")
	if first < 0 || second < 0 {
		t.Fatalf("picker must show both session titles:\n%s", out)
	}
	if first > second {
		t.Fatalf("picker must list newest-first (second question above first):\n%s", out)
	}
	if !strings.Contains(out, "2 msgs") || !strings.Contains(out, "just now") {
		t.Fatalf("picker rows must carry message count + relative time:\n%s", out)
	}
}

func TestModelSessionsResumeContinuesSameSessionID(t *testing.T) {
	dir := t.TempDir()
	st := seedStore(t, dir, "why did cost spike?")
	metas, err := st.List()
	if err != nil || len(metas) != 1 {
		t.Fatalf("seed: %v %v", metas, err)
	}
	wantID := metas[0].ID

	m, rr := newTestModel(testSessionLocal(), noTurn)
	m.openSessions = func() (SessionBrowser, error) { return st, nil }
	m.width, m.height = 120, 40
	m = openPickerViaTypedCommand(t, m)

	// enter resumes the highlighted session…
	m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on a session row must emit the resume command")
	}
	m, _ = updateModel(m, cmd())
	if m.sessionsOpen {
		t.Fatal("a successful resume must close the picker")
	}
	// …re-rendering the saved transcript (user + assistant text turns)…
	transcript := stripSGR(m.conv.View(110, 30))
	if !strings.Contains(transcript, "why did cost spike?") || !strings.Contains(transcript, "answer to why did cost spike?") {
		t.Fatalf("resume must replay the saved user/assistant turns:\n%s", transcript)
	}
	if !strings.Contains(transcript, "resumed past conversation (2 saved messages)") {
		t.Fatalf("resume must announce itself in the transcript:\n%s", transcript)
	}
	// …and the NEXT turn continues the SAME session id: the agent build seam
	// receives the loaded session (the kernel then appends to it — pinned by
	// runtime.TestBuildAgentResumesInjectedSession).
	m.conv.input.SetValue("and now?")
	m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = pumpModel(m, cmd)
	if rr.gotText != "and now?" {
		t.Fatalf("the next turn must run, got %q", rr.gotText)
	}
	if rr.gotResume == nil || rr.gotResume.ID != wantID {
		t.Fatalf("the next turn must be built on the resumed session %s, got %+v", wantID, rr.gotResume)
	}
}

func TestModelSessionsDeleteRemovesDurably(t *testing.T) {
	dir := t.TempDir()
	st := seedStore(t, dir, "old chat", "newer chat")
	m, _ := newTestModel(testSessionLocal(), noTurn)
	m.openSessions = func() (SessionBrowser, error) { return st, nil }
	m.width, m.height = 120, 40
	m = openPickerViaTypedCommand(t, m)

	// The cursor starts on the newest ("newer chat"); d deletes it.
	metas, _ := st.List()
	doomed := metas[0].ID
	m, _ = updateModel(m, keyCtrlD())
	out := stripSGR(m.footerBar(120))
	if strings.Contains(out, "newer chat") {
		t.Fatalf("the deleted session must leave the listing:\n%s", out)
	}
	if !strings.Contains(out, "old chat") {
		t.Fatalf("the other session must survive:\n%s", out)
	}
	// Durable: gone from the on-disk store, not just the overlay.
	if _, err := st.Load(doomed); err == nil {
		t.Fatal("d must delete the session from the store, not just the list")
	}
	if left, _ := st.List(); len(left) != 1 {
		t.Fatalf("exactly one session must remain on disk, got %d", len(left))
	}
}

func TestModelSessionsEmptyStoreShowsNamedEmptyState(t *testing.T) {
	st := agent.OpenStoreAt(t.TempDir()) // nothing saved
	m, _ := newTestModel(testSessionLocal(), noTurn)
	m.openSessions = func() (SessionBrowser, error) { return st, nil }
	m.width, m.height = 120, 40
	m = openPickerViaTypedCommand(t, m)
	if !m.sessionsOpen {
		t.Fatal("an empty store still opens the picker (with its named empty state)")
	}
	out := stripSGR(m.footerBar(120))
	if !strings.Contains(out, "no saved conversations yet") {
		t.Fatalf("an empty store must show the named empty state, not a blank picker:\n%s", out)
	}
}

func TestModelSessionsEscClosesPicker(t *testing.T) {
	st := seedStore(t, t.TempDir(), "a chat")
	m, _ := newTestModel(testSessionLocal(), noTurn)
	m.openSessions = func() (SessionBrowser, error) { return st, nil }
	m = openPickerViaTypedCommand(t, m)
	m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc must emit the close command")
	}
	m, _ = updateModel(m, cmd())
	if m.sessionsOpen {
		t.Fatal("esc must close the picker")
	}
}

func TestModelSessionsFailureModesAreNamed(t *testing.T) {
	// No seam wired (tests / single-shot builds): a named notice, no picker.
	m, _ := newTestModel(testSessionLocal(), noTurn)
	m, _ = updateModel(m, openSessionsMsg{})
	if m.sessionsOpen || !strings.Contains(m.conv.notice, "session history isn't available") {
		t.Fatalf("a nil seam must surface the named notice, got %q", m.conv.notice)
	}

	// The store can't open.
	m2, _ := newTestModel(testSessionLocal(), noTurn)
	m2.openSessions = func() (SessionBrowser, error) { return nil, errors.New("no config dir") }
	m2, _ = updateModel(m2, openSessionsMsg{})
	if m2.sessionsOpen || !strings.Contains(m2.conv.notice, "couldn't open session history: no config dir") {
		t.Fatalf("an open failure must be named, got %q", m2.conv.notice)
	}

	// Listing fails.
	m3, _ := newTestModel(testSessionLocal(), noTurn)
	m3.openSessions = func() (SessionBrowser, error) { return &fakeBrowser{listErr: errors.New("disk gone")}, nil }
	m3, _ = updateModel(m3, openSessionsMsg{})
	if m3.sessionsOpen || !strings.Contains(m3.conv.notice, "couldn't list sessions: disk gone") {
		t.Fatalf("a list failure must be named, got %q", m3.conv.notice)
	}

	// The picked session fails to load: the picker stays open and names it.
	m4, _ := newTestModel(testSessionLocal(), noTurn)
	br := &fakeBrowser{metas: []agent.SessionMeta{{ID: "s1", Title: "a chat", Messages: 2}}, loadErr: errors.New("corrupt file")}
	m4.openSessions = func() (SessionBrowser, error) { return br, nil }
	m4, _ = updateModel(m4, openSessionsMsg{})
	m4, _ = updateModel(m4, sessionResumeMsg{id: "s1"})
	if !m4.sessionsOpen {
		t.Fatal("a failed load must keep the picker open")
	}
	if out := stripSGR(m4.footerBar(120)); !strings.Contains(out, "couldn't load: corrupt file") {
		t.Fatalf("a load failure must be named in the picker:\n%s", out)
	}

	// A stray resume with no picker open is a safe no-op (no nil deref).
	m5, _ := newTestModel(testSessionLocal(), noTurn)
	if m5, _ = updateModel(m5, sessionResumeMsg{id: "ghost"}); m5.sessionsOpen {
		t.Fatal("a stray resume must not open anything")
	}

	// Delete fails: the row stays and the error is named.
	p := newSessionPicker(&fakeBrowser{delErr: errors.New("file locked")},
		[]agent.SessionMeta{{ID: "s1", Title: "a chat"}})
	p, _ = p.Update(keyCtrlD())
	if len(p.metas) != 1 || !strings.Contains(p.err, "couldn't delete: file locked") {
		t.Fatalf("a failed delete must keep the row and name the error, got metas=%d err=%q", len(p.metas), p.err)
	}
}

func TestSessionPickerNavigationAndCap(t *testing.T) {
	metas := make([]agent.SessionMeta, 60)
	for i := range metas {
		metas[i] = agent.SessionMeta{ID: fmt.Sprintf("s%d", i), Title: fmt.Sprintf("chat %d", i)}
	}
	p := newSessionPicker(&fakeBrowser{}, metas)
	if len(p.metas) != sessionListCap {
		t.Fatalf("the picker caps at the %d newest sessions, got %d", sessionListCap, len(p.metas))
	}
	// ↑ at the top stays put; ↓ moves (letters now edit the filter instead).
	p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if p.cursor != 0 {
		t.Fatal("up at the top must stay at 0")
	}
	p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if p.cursor != 1 {
		t.Fatalf("down/down/up must land the cursor on 1, got %d", p.cursor)
	}
	// enter on the highlighted row resumes THAT session.
	p2, cmd := p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter must emit a resume")
	}
	if msg, ok := cmd().(sessionResumeMsg); !ok || msg.id != "s1" {
		t.Fatalf("enter must resume the highlighted session s1, got %#v", cmd())
	}
	// enter/d on an empty picker are no-ops (no panic, no command).
	empty := newSessionPicker(&fakeBrowser{}, nil)
	if _, cmd := empty.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatal("enter on an empty picker must be a no-op")
	}
	if _, cmd := empty.Update(keyCtrlD()); cmd != nil {
		t.Fatal("ctrl+d on an empty picker must be a no-op")
	}
	_ = p2
}

func TestSessionPickerDeleteClampsCursor(t *testing.T) {
	st := seedStore(t, t.TempDir(), "one", "two")
	metas, _ := st.List()
	p := newSessionPicker(st, metas)
	// Move to the LAST row, delete it: the cursor clamps onto the remaining row.
	p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	p, _ = p.Update(keyCtrlD())
	if len(p.metas) != 1 || p.cursor != 0 {
		t.Fatalf("deleting the last row must clamp the cursor, got %d metas cursor=%d", len(p.metas), p.cursor)
	}
	// Deleting the final row leaves the named empty state, cursor pinned at 0.
	p, _ = p.Update(keyCtrlD())
	if len(p.metas) != 0 || p.cursor != 0 {
		t.Fatalf("deleting everything must leave an empty picker, got %d metas cursor=%d", len(p.metas), p.cursor)
	}
	if !strings.Contains(stripSGR(p.View()), "no saved conversations yet") {
		t.Fatal("an emptied picker must show the named empty state")
	}
}

func TestConversationHistoryAliasAndClearResetsResume(t *testing.T) {
	c, rr := newTestConv(testSessionLocal(), noTurn)
	// /history is an alias of /sessions.
	cmd := c.agentCommand("/history")
	if cmd == nil {
		t.Fatal("/history must emit the open-picker command")
	}
	if _, ok := cmd().(openSessionsMsg); !ok {
		t.Fatal("/history must request the session picker")
	}

	// A resumed session rides into the agent build; /clear abandons it so the
	// next turn starts a FRESH session again.
	sess := agent.NewSession("local")
	sess.Messages = []agent.Message{agent.TextMessage(agent.RoleUser, "earlier")}
	c.resumeSession(sess)
	c.input.SetValue("continue")
	pumpConv(c, c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}))
	if rr.gotResume != sess {
		t.Fatal("after a resume, the build seam must receive the loaded session")
	}
	c.input.SetValue("/clear")
	c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	c.input.SetValue("fresh start")
	pumpConv(c, c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}))
	if rr.gotResume != nil {
		t.Fatal("/clear must abandon the resumed session — the rebuilt agent starts fresh")
	}
}

func TestConversationResumeInterruptsInFlightTurn(t *testing.T) {
	// Resuming while a turn streams must cancel it (like /clear) so the dead
	// turn's terminal noise never lands in the restored transcript.
	c, _ := newTestConv(testSessionLocal(), noTurn)
	c.running = true
	sess := agent.NewSession("local")
	sess.Messages = []agent.Message{
		agent.TextMessage(agent.RoleUser, "old topic"),
		// A pure tool exchange: the tool_use turn and its tool_result reply carry
		// no text blocks, so neither replays as a transcript line.
		{Role: agent.RoleAssistant, Blocks: []agent.Block{{Type: agent.BlockToolUse, ID: "t1", ToolName: "observe_health"}}},
		{Role: agent.RoleUser, Blocks: []agent.Block{agent.ToolResult("t1", "27 nodes healthy", false)}},
	}
	c.resumeSession(sess)
	if !c.interrupted {
		t.Fatal("resuming mid-turn must mark the in-flight turn interrupted")
	}
	if c.running {
		t.Fatal("resetAgent must clear the running turn")
	}
	out := stripSGR(c.View(100, 24))
	if !strings.Contains(out, "old topic") {
		t.Fatalf("the resumed transcript must replace the interrupted one:\n%s", out)
	}
	if strings.Contains(out, "27 nodes healthy") {
		t.Fatalf("tool-result turns must not replay as transcript lines:\n%s", out)
	}
}

func TestSessionsSlashPaletteEntry(t *testing.T) {
	// The palette carries /sessions with the /history alias, so both fuzzy
	// queries land on it.
	cmds := defaultSlashCommands()
	hits := matchSlash(cmds, "history")
	found := false
	for _, c := range hits {
		if c.name == "sessions" && c.kind == slashAgent {
			found = true
		}
	}
	if !found {
		t.Fatal("the slash palette must expose /sessions (alias history) as an agent command")
	}
}

func TestShellWiresOpenSessionsIntoDashboard(t *testing.T) {
	// The CLI's Deps.OpenSessions must reach the dashboard (wireDash), or
	// /sessions would report "unavailable" on every real build.
	st := agent.OpenStoreAt(t.TempDir())
	d := Deps{
		Gateway:      sampleGateway(),
		Session:      Session{EnvName: "local", Model: "m", VKSecret: "vk"},
		HasSession:   func() bool { return true },
		OpenSessions: func() (SessionBrowser, error) { return st, nil },
	}
	s := NewShell(d)
	if s.inWizard {
		t.Fatal("precondition: a complete session must skip the wizard")
	}
	if s.dash.openSessions == nil {
		t.Fatal("NewShell must wire Deps.OpenSessions into the dashboard")
	}
	if br, err := s.dash.openSessions(); err != nil || br == nil {
		t.Fatalf("the wired seam must resolve the browser, got %v err=%v", br, err)
	}
}

func TestSessionPickerShowsWebBirthplaceBadge(t *testing.T) {
	// A session synced down from the web chat (env "web") is marked [web] so the
	// operator can tell cloud-born conversations from device-born ones; local
	// sessions carry no badge.
	p := newSessionPicker(&fakeBrowser{}, []agent.SessionMeta{
		{ID: "w1", Env: "web", Title: "asked on the web", Messages: 3},
		{ID: "l1", Env: "local", Title: "asked here", Messages: 2},
	})
	out := stripSGR(p.View())
	webRow, localRow := "", ""
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "asked on the web") {
			webRow = line
		}
		if strings.Contains(line, "asked here") {
			localRow = line
		}
	}
	if !strings.Contains(webRow, "[web]") {
		t.Fatalf("a web-born session must carry the [web] badge:\n%s", out)
	}
	if strings.Contains(localRow, "[web]") {
		t.Fatalf("a device-born session must NOT carry the [web] badge:\n%s", out)
	}
}

func TestRelTimeBuckets(t *testing.T) {
	now := time.Now()
	cases := []struct {
		at   time.Time
		want string
	}{
		{now.Add(-10 * time.Second), "just now"},
		{now.Add(-5 * time.Minute), "5m ago"},
		{now.Add(-3 * time.Hour), "3h ago"},
		{now.Add(-49 * time.Hour), "2d ago"},
	}
	for _, c := range cases {
		if got := relTime(c.at); got != c.want {
			t.Fatalf("relTime(%v) = %q, want %q", c.at, got, c.want)
		}
	}
}

// keyCtrlD builds the picker's delete chord (typing now edits the filter, so
// plain d is a filter character).
func keyCtrlD() tea.KeyPressMsg { return tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl} }

// TestSessionPickerTypeToFilter pins the live filter: typing narrows the list
// to matching titles (case-insensitive), backspace widens it again, a miss
// names itself, and esc clears the filter before a second esc closes.
func TestSessionPickerTypeToFilter(t *testing.T) {
	p := newSessionPicker(&fakeBrowser{}, []agent.SessionMeta{
		{ID: "a", Title: "cost spike in prod"},
		{ID: "b", Title: "deploy the gateway"},
		{ID: "c", Title: "why did COSTS rise"},
	})
	for _, r := range "cost" {
		p, _ = p.Update(keyRunes(string(r)))
	}
	if len(p.metas) != 2 {
		t.Fatalf("filter 'cost' must match the two cost titles, got %d", len(p.metas))
	}
	out := stripSGR(p.View())
	if !strings.Contains(out, "filter: cost") {
		t.Fatalf("the live filter must be visible:\n%s", out)
	}
	if strings.Contains(out, "deploy the gateway") {
		t.Fatalf("non-matching rows must be hidden:\n%s", out)
	}
	// enter resumes the highlighted FILTERED row, not the unfiltered index.
	p2, cmd := p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if msg, ok := cmd().(sessionResumeMsg); !ok || msg.id != "a" {
		t.Fatalf("enter must resume the first filtered match, got %#v", cmd())
	}
	_ = p2
	// A miss names itself and backspace widens.
	for _, r := range "zzz" {
		p, _ = p.Update(keyRunes(string(r)))
	}
	if out := stripSGR(p.View()); !strings.Contains(out, "no conversations match") {
		t.Fatalf("a filter miss must name itself:\n%s", out)
	}
	p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if p.filter != "costzz" {
		t.Fatalf("backspace must trim the filter, got %q", p.filter)
	}
	// esc clears the filter first; only a second esc closes.
	p, cmd = p.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd != nil || p.filter != "" {
		t.Fatalf("esc with a filter must clear it, not close (filter=%q)", p.filter)
	}
	if _, cmd = p.Update(tea.KeyPressMsg{Code: tea.KeyEsc}); cmd == nil {
		t.Fatal("esc with no filter must close the picker")
	}
}

// TestSessionPickerWindowsAroundCursor pins the viewport: a long list renders
// only a window around the cursor with explicit "… N more" markers, and the
// 50-session cap announces what it hides — never a silent cut.
func TestSessionPickerWindowsAroundCursor(t *testing.T) {
	metas := make([]agent.SessionMeta, 60)
	for i := range metas {
		metas[i] = agent.SessionMeta{ID: fmt.Sprintf("s%d", i), Title: fmt.Sprintf("chat number %02d", i)}
	}
	p := newSessionPicker(&fakeBrowser{}, metas)
	out := stripSGR(p.View())
	if !strings.Contains(out, "more below") {
		t.Fatalf("a long list must mark the hidden tail:\n%s", out)
	}
	if !strings.Contains(out, "showing the newest 50 of 60") {
		t.Fatalf("the cap must announce what it hides:\n%s", out)
	}
	if strings.Contains(out, "chat number 20") {
		t.Fatalf("rows beyond the window must not render:\n%s", out)
	}
	// Walk the cursor deep into the list: the window follows it.
	for i := 0; i < 30; i++ {
		p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	out = stripSGR(p.View())
	if !strings.Contains(out, "chat number 30") {
		t.Fatalf("the window must follow the cursor:\n%s", out)
	}
	if !strings.Contains(out, "more above") || !strings.Contains(out, "more below") {
		t.Fatalf("a mid-list window must mark both hidden sides:\n%s", out)
	}
}
