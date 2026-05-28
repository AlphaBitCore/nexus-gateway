package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// Deps is everything the entry shell needs that the dashboard does not: the
// auth + persistence callbacks, injected so the wizard is testable without a
// browser, keychain, or disk. The funcs are nil-safe (a nil Login means "no
// interactive login available", surfaced as an error rather than a panic).
type Deps struct {
	Gateway    Gateway
	Session    Session
	EnvNames   []string // configured environments, for the entry env step
	HasSession func() bool
	// SwitchEnv re-resolves to an existing environment, rebuilds the gateway, and
	// reports whether that env already has a stored credential. Nil disables the
	// env step (single-env builds / tests).
	SwitchEnv func(name string) (Gateway, Session, bool, error)
	// CreateEnv persists a new environment (name + Control Plane URL + prod flag,
	// other fields defaulted), sets it as default, and switches to it.
	CreateEnv     func(name, cpBaseURL string, prod bool) (Gateway, Session, error)
	Login         func(context.Context) error
	SaveVKSecret  func(secret string) error
	SaveSelection func(model, vkID, vkName string) error
	// CreateVK creates a personal Virtual Key the operator owns and returns its
	// (id, name, plaintext secret). It is how an operator without a key gets one
	// without borrowing anyone else's traffic. Injected for testability.
	CreateVK func(ctx context.Context, name string) (id, name2, secret string, err error)
}

// wizardStage is the wizard's linear step.
type wizardStage int

const (
	stageEnv     wizardStage = iota // pick an existing environment or create one
	stageEnvName                    // create: enter the new environment's name
	stageEnvURL                     // create: enter the Control Plane URL
	stageLogin
	stageModel
	stageVK
	stageSecret
)

// modelChoice is one selectable model row (flattened from the grouped catalog).
type modelChoice struct {
	code     string
	name     string
	provider string
}

// wizard is the first-run entry flow: login → pick model → pick VK + secret.
// It runs only when the stored selection is missing/invalid; otherwise the
// shell goes straight to the dashboard (FR-13).
type wizard struct {
	deps    Deps
	session Session
	stage   wizardStage

	// gateway is the live client; it starts as deps.Gateway and is replaced when
	// the env step switches/creates an environment (so later steps + the
	// dashboard talk to the chosen deployment).
	gateway Gateway

	loggingIn bool
	loading   bool

	envCursor  int    // selection in the env list ([len] == the "create new" row)
	newEnvName string // captured during the create flow

	models      []modelChoice
	modelCursor int

	vks      []core.VirtualKey
	vkCursor int

	input  textinput.Model // env name / URL entry (create flow)
	secret textinput.Model
	err    error
	done   bool
}

type loginDoneMsg struct{ err error }
type wizModelsMsg struct {
	models []modelChoice
	err    error
}
type wizVKsMsg struct {
	vks []core.VirtualKey
	err error
}
type vkCreatedMsg struct {
	id, name, secret string
	err              error
}

func newWizard(d Deps) *wizard {
	ti := textinput.New()
	ti.Placeholder = "paste the Virtual Key secret (nvk_…)"
	ti.EchoMode = textinput.EchoPassword
	ti.CharLimit = 200
	ti.Width = 50
	in := textinput.New()
	in.CharLimit = 200
	in.Width = 50
	return &wizard{deps: d, session: d.Session, gateway: d.Gateway, secret: ti, input: in}
}

// Init starts at the env step when more than one environment is configurable
// (so the operator confirms/switches the target first); otherwise it preserves
// the original flow: straight to model selection if a session already exists,
// else login.
func (w *wizard) Init() tea.Cmd {
	w.gateway = w.deps.Gateway
	if w.deps.SwitchEnv != nil && len(w.deps.EnvNames) > 0 {
		w.stage = stageEnv
		w.envCursor = w.indexOfCurrentEnv()
		return nil
	}
	if w.deps.HasSession != nil && w.deps.HasSession() {
		w.stage = stageModel
		w.loading = true
		return w.fetchModels()
	}
	w.stage = stageLogin
	return nil
}

// indexOfCurrentEnv preselects the active environment in the env list.
func (w *wizard) indexOfCurrentEnv() int {
	for i, n := range w.deps.EnvNames {
		if n == w.session.EnvName {
			return i
		}
	}
	return 0
}

func (w *wizard) Update(msg tea.Msg) (*wizard, tea.Cmd) {
	switch msg := msg.(type) {
	case loginDoneMsg:
		w.loggingIn = false
		if msg.err != nil {
			w.err = msg.err
			return w, nil
		}
		w.err = nil
		w.stage = stageModel
		w.loading = true
		return w, w.fetchModels()
	case wizModelsMsg:
		w.loading = false
		w.err = msg.err
		w.models = msg.models
		return w, nil
	case wizVKsMsg:
		w.loading = false
		w.err = msg.err
		w.vks = msg.vks
		return w, nil
	case vkCreatedMsg:
		w.loading = false
		if msg.err != nil {
			w.err = fmt.Errorf("create VK: %w", msg.err)
			return w, nil
		}
		w.session.VKID, w.session.VKName = msg.id, msg.name
		w.session.VKSecret = msg.secret
		if w.deps.SaveVKSecret != nil {
			if err := w.deps.SaveVKSecret(msg.secret); err != nil {
				w.err = fmt.Errorf("store VK secret: %w", err)
				return w, nil
			}
		}
		if w.deps.SaveSelection != nil {
			if err := w.deps.SaveSelection(w.session.Model, w.session.VKID, w.session.VKName); err != nil {
				w.err = fmt.Errorf("save selection: %w", err)
				return w, nil
			}
		}
		w.done = true
		return w, nil
	case tea.KeyMsg:
		return w.updateKey(msg)
	}
	return w, nil
}

func (w *wizard) updateKey(msg tea.KeyMsg) (*wizard, tea.Cmd) {
	switch w.stage {
	case stageEnv:
		switch msg.String() {
		case "up", "k":
			if w.envCursor > 0 {
				w.envCursor--
			}
		case "down", "j":
			if w.envCursor < len(w.deps.EnvNames) { // last index = the "create new" row
				w.envCursor++
			}
		case "enter":
			return w.selectEnv()
		}
	case stageEnvName:
		if msg.Type == tea.KeyEnter {
			name := strings.TrimSpace(w.input.Value())
			if name == "" {
				w.err = fmt.Errorf("environment name is required")
				return w, nil
			}
			w.err = nil
			w.newEnvName = name
			w.input.SetValue("")
			w.input.Placeholder = "Control Plane base URL (https://…)"
			w.stage = stageEnvURL
			return w, w.input.Focus()
		}
		var cmd tea.Cmd
		w.input, cmd = w.input.Update(msg)
		return w, cmd
	case stageEnvURL:
		if msg.Type == tea.KeyEnter {
			return w.createEnv()
		}
		var cmd tea.Cmd
		w.input, cmd = w.input.Update(msg)
		return w, cmd
	case stageLogin:
		if msg.Type == tea.KeyEnter && !w.loggingIn {
			return w.startLogin()
		}
	case stageModel:
		switch msg.String() {
		case "up", "k":
			if w.modelCursor > 0 {
				w.modelCursor--
			}
		case "down", "j":
			if w.modelCursor < len(w.models)-1 {
				w.modelCursor++
			}
		case "enter":
			if w.modelCursor < len(w.models) {
				w.session.Model = w.models[w.modelCursor].code
				w.stage = stageVK
				w.loading = true
				return w, w.fetchVKs()
			}
		}
	case stageVK:
		switch msg.String() {
		case "up", "k":
			if w.vkCursor > 0 {
				w.vkCursor--
			}
		case "down", "j":
			if w.vkCursor < len(w.vks)-1 {
				w.vkCursor++
			}
		case "c":
			// Create a brand-new VK the operator owns (no borrowed traffic).
			return w.startCreateVK()
		case "enter":
			if w.vkCursor < len(w.vks) {
				w.session.VKID = w.vks[w.vkCursor].ID
				w.session.VKName = w.vks[w.vkCursor].Name
				w.stage = stageSecret
				return w, w.secret.Focus()
			}
		}
	case stageSecret:
		if msg.Type == tea.KeyEnter {
			return w.finish()
		}
		var cmd tea.Cmd
		w.secret, cmd = w.secret.Update(msg)
		return w, cmd
	}
	return w, nil
}

// selectEnv switches to the chosen existing environment (rebuilding the
// gateway) or, on the "create new" row, enters the create flow. A switched env
// that is already fully configured finishes the wizard immediately.
func (w *wizard) selectEnv() (*wizard, tea.Cmd) {
	if w.envCursor >= len(w.deps.EnvNames) {
		w.stage = stageEnvName
		w.err = nil
		w.input.SetValue("")
		w.input.Placeholder = "new environment name (e.g. dev)"
		return w, w.input.Focus()
	}
	gw, sess, loggedIn, err := w.deps.SwitchEnv(w.deps.EnvNames[w.envCursor])
	if err != nil {
		w.err = err
		return w, nil
	}
	w.err = nil
	w.gateway = gw
	w.session = sess
	return w, w.afterEnv(loggedIn)
}

// createEnv persists + switches to the new environment, then proceeds to login.
func (w *wizard) createEnv() (*wizard, tea.Cmd) {
	url := strings.TrimSpace(w.input.Value())
	if url == "" {
		w.err = fmt.Errorf("Control Plane URL is required")
		return w, nil
	}
	if w.deps.CreateEnv == nil {
		w.err = fmt.Errorf("creating an environment is not available in this build")
		return w, nil
	}
	gw, sess, err := w.deps.CreateEnv(w.newEnvName, url, false)
	if err != nil {
		w.err = fmt.Errorf("create environment: %w", err)
		return w, nil
	}
	w.err = nil
	w.gateway = gw
	w.session = sess
	w.stage = stageLogin // a new environment is never logged in yet
	return w, nil
}

// afterEnv routes past the env step: a fully-configured logged-in env finishes
// the wizard; a logged-in env still needing a selection goes to model; otherwise
// the operator logs in.
func (w *wizard) afterEnv(loggedIn bool) tea.Cmd {
	if loggedIn && w.session.Model != "" && strings.TrimSpace(w.session.VKSecret) != "" {
		w.done = true
		return nil
	}
	if loggedIn {
		w.stage = stageModel
		w.loading = true
		return w.fetchModels()
	}
	w.stage = stageLogin
	return nil
}

// startLogin runs the injected browser login as a command.
func (w *wizard) startLogin() (*wizard, tea.Cmd) {
	if w.deps.Login == nil {
		w.err = fmt.Errorf("no interactive login configured for this environment")
		return w, nil
	}
	w.loggingIn = true
	w.err = nil
	login := w.deps.Login
	return w, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
		defer cancel()
		return loginDoneMsg{err: login(ctx)}
	}
}

// startCreateVK creates a personal VK the operator owns and finishes the wizard
// with the once-returned plaintext (no need to paste a secret).
func (w *wizard) startCreateVK() (*wizard, tea.Cmd) {
	if w.deps.CreateVK == nil {
		w.err = fmt.Errorf("creating a Virtual Key is not available in this build")
		return w, nil
	}
	w.loading = true
	w.err = nil
	create := w.deps.CreateVK
	return w, func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		id, name, secret, err := create(ctx, "nexus-cli")
		return vkCreatedMsg{id: id, name: name, secret: secret, err: err}
	}
}

// finish validates the secret, persists it + the selection, and signals done.
func (w *wizard) finish() (*wizard, tea.Cmd) {
	secret := strings.TrimSpace(w.secret.Value())
	if secret == "" {
		w.err = fmt.Errorf("a Virtual Key secret is required to chat / run the lab")
		return w, nil
	}
	if w.deps.SaveVKSecret != nil {
		if err := w.deps.SaveVKSecret(secret); err != nil {
			w.err = fmt.Errorf("store VK secret: %w", err)
			return w, nil
		}
	}
	if w.deps.SaveSelection != nil {
		if err := w.deps.SaveSelection(w.session.Model, w.session.VKID, w.session.VKName); err != nil {
			w.err = fmt.Errorf("save selection: %w", err)
			return w, nil
		}
	}
	w.session.VKSecret = secret
	w.done = true
	return w, nil
}

func (w *wizard) fetchModels() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		cat, err := w.gateway.AdminModels(ctx)
		if err != nil {
			return wizModelsMsg{err: err}
		}
		return wizModelsMsg{models: flattenModels(cat)}
	}
}

func (w *wizard) fetchVKs() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		vks, err := w.gateway.VirtualKeys(ctx)
		if err != nil {
			return wizVKsMsg{err: err}
		}
		return wizVKsMsg{vks: enabledVKs(vks)}
	}
}

// flattenModels turns the grouped catalog into a flat, selectable list of
// enabled chat models (the Playground + lab only call chat completions).
func flattenModels(cat *core.ModelCatalog) []modelChoice {
	var out []modelChoice
	if cat == nil {
		return out
	}
	for _, g := range cat.Data {
		for _, m := range g.Models {
			if !m.Enabled || (m.Type != "" && m.Type != "chat") {
				continue
			}
			out = append(out, modelChoice{code: m.Code, name: m.Name, provider: g.Provider.Label()})
		}
	}
	return out
}

// enabledVKs keeps only usable keys so the operator can't pick a disabled one.
func enabledVKs(vks []core.VirtualKey) []core.VirtualKey {
	out := make([]core.VirtualKey, 0, len(vks))
	for _, v := range vks {
		if v.Enabled {
			out = append(out, v)
		}
	}
	return out
}

func (w *wizard) View(width, height int) string {
	var b strings.Builder
	b.WriteString(styles.StatusBar.Render("nexus setup · " + w.session.EnvName))
	b.WriteString("\n\n")
	switch w.stage {
	case stageEnv:
		b.WriteString(styles.TileValue.Render("Step 0 — Choose an environment"))
		b.WriteString("\n")
		b.WriteString(styles.TileLabel.Render("enter = use selected · ↑/↓ to move\n"))
		b.WriteString(w.renderEnvs(height - 6))
	case stageEnvName:
		b.WriteString(styles.TileValue.Render("New environment — name"))
		b.WriteString("\n")
		b.WriteString(w.input.View())
		b.WriteString("\n")
		b.WriteString(styles.TileLabel.Render("enter to continue"))
	case stageEnvURL:
		b.WriteString(styles.TileValue.Render("New environment — Control Plane URL"))
		b.WriteString("\n")
		b.WriteString(styles.TileLabel.Render(fmt.Sprintf("%q · enter the Control Plane base URL\n", w.newEnvName)))
		b.WriteString(w.input.View())
		b.WriteString("\n")
		b.WriteString(styles.TileLabel.Render("enter to create (other fields default; edit later via `nexus setup`)"))
	case stageLogin:
		b.WriteString(styles.TileValue.Render("Step 1 — Log in"))
		b.WriteString("\n")
		if w.loggingIn {
			b.WriteString(styles.TileLabel.Render("opening browser… complete the login, then return here"))
		} else {
			b.WriteString(styles.TileLabel.Render("press enter to log in via your browser (OAuth2 + PKCE)"))
		}
	case stageModel:
		b.WriteString(styles.TileValue.Render("Step 2 — Pick a model"))
		b.WriteString("\n")
		b.WriteString(w.renderModels(height - 6))
	case stageVK:
		b.WriteString(styles.TileValue.Render("Step 3 — Pick a Virtual Key (or press c to create your own)"))
		b.WriteString("\n")
		b.WriteString(styles.TileLabel.Render("enter = use selected (you'll paste its secret) · c = create a new key you own\n"))
		b.WriteString(w.renderVKs(height - 7))
	case stageSecret:
		b.WriteString(styles.TileValue.Render("Step 4 — Paste the VK secret"))
		b.WriteString("\n")
		b.WriteString(styles.TileLabel.Render(fmt.Sprintf("key %q — its secret is shown once at creation and is not stored server-side.\n", w.session.VKName)))
		b.WriteString(w.secret.View())
		b.WriteString("\n")
		b.WriteString(styles.TileLabel.Render("enter to save (stored in the OS keychain, never on disk)"))
	}
	if w.loading {
		b.WriteString("\n")
		b.WriteString(styles.TileLabel.Render("loading…"))
	}
	if w.err != nil {
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ " + w.err.Error()))
	}
	return b.String()
}

// renderEnvs renders the env list with a trailing "create new" row.
func (w *wizard) renderEnvs(budget int) string {
	n := len(w.deps.EnvNames)
	return renderCursorList(budget, w.envCursor, n+1, func(i int) string {
		if i == n {
			return "＋ create a new environment"
		}
		return w.deps.EnvNames[i]
	})
}

func (w *wizard) renderModels(budget int) string {
	if len(w.models) == 0 {
		if w.loading {
			return ""
		}
		return styles.TileLabel.Render("  (no chat models configured)")
	}
	return renderCursorList(budget, w.modelCursor, len(w.models), func(i int) string {
		m := w.models[i]
		return fmt.Sprintf("%-26s %s", m.code, styles.TileLabel.Render(m.provider))
	})
}

func (w *wizard) renderVKs(budget int) string {
	if len(w.vks) == 0 {
		if w.loading {
			return ""
		}
		return styles.TileLabel.Render("  (no enabled virtual keys — create one in the web UI)")
	}
	return renderCursorList(budget, w.vkCursor, len(w.vks), func(i int) string {
		v := w.vks[i]
		return fmt.Sprintf("%-28s %s", v.Name, styles.TileLabel.Render(v.KeyPrefix))
	})
}

// renderCursorList renders a windowed, cursor-highlighted list of n rows.
func renderCursorList(budget, cursor, n int, row func(i int) string) string {
	if budget < 3 {
		budget = 3
	}
	start := 0
	if cursor >= budget {
		start = cursor - budget + 1
	}
	end := start + budget
	if end > n {
		end = n
	}
	var lines []string
	for i := start; i < end; i++ {
		prefix := "  "
		line := row(i)
		if i == cursor {
			prefix = lipgloss.NewStyle().Foreground(styles.Brand).Render("▸ ")
			line = lipgloss.NewStyle().Bold(true).Render(line)
		}
		lines = append(lines, prefix+line)
	}
	return strings.Join(lines, "\n")
}
