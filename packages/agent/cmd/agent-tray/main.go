// Command agent-tray is the Windows / Linux equivalent of the macOS
// menu-bar app. It runs as a user-level process
// alongside the agent daemon, polls the daemon's statusapi for state,
// and renders the same seven-item menu the macOS Swift app exposes:
//
//   ● Protection Active            (status row, disabled)
//   Open Dashboard                 (launches the Wails app)
//   <SSO identity, when known>     (disabled)
//   Pause Protection ▸             (submenu: 15m / 1h / 8h / Indefinite)
//     —— or, when paused ——
//   Resume Protection
//   Settings…                      (opens Dashboard at /settings)
//   About Nexus Agent
//   Restart Agent
//   Quit Nexus Agent
//
// macOS continues to use the SwiftUI NSStatusItem app — this binary
// is only built / installed on linux and windows. Build tags below
// gate the file accordingly.
//
//go:build linux || windows

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"fyne.io/systray"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/host/trayipc"
)

var version = "0.0.0-dev"

func main() {
	dashboardPath := flag.String("dashboard", "",
		"override path to the Nexus Agent Dashboard binary (default: search standard install locations)")
	flag.Parse()

	app := &trayApp{
		client:        trayipc.NewClient(),
		dashboardPath: *dashboardPath,
		logger:        slog.Default(),
	}
	slog.Info("nexus-agent-tray starting", "version", version, "os", runtime.GOOS)

	// Run is the blocking main loop. The systray package owns the
	// platform's GUI thread; everything else lives in goroutines.
	systray.Run(app.onReady, app.onExit)
}

// trayApp owns the menu items + the poll loop. The fields are written
// from onReady (main goroutine) and read from the poll loop (separate
// goroutine), so mutations on the systray side are serialised by
// fyne.io/systray itself; we only need a mutex to protect cached
// state for the change-detector.
type trayApp struct {
	client        *trayipc.Client
	dashboardPath string
	logger        *slog.Logger

	// Menu items, populated in onReady and updated on each poll.
	status     *systray.MenuItem
	identity   *systray.MenuItem
	openDash   *systray.MenuItem
	pauseMenu  *systray.MenuItem
	resumeItem *systray.MenuItem
	pause15    *systray.MenuItem
	pause1h    *systray.MenuItem
	pause8h    *systray.MenuItem
	pauseInf   *systray.MenuItem
	settings   *systray.MenuItem
	about      *systray.MenuItem
	restart    *systray.MenuItem
	quit       *systray.MenuItem

	mu         sync.Mutex
	lastState  string
	lastPaused bool
	lastSSO    string
	pollCancel context.CancelFunc
}

func (a *trayApp) onReady() {
	systray.SetTitle("Nexus Agent")
	systray.SetTooltip("Nexus Agent")
	a.applyIcon(stateError) // pessimistic until first poll

	// 1. Status row (disabled, repurposed as a label).
	a.status = systray.AddMenuItem("Connecting…", "")
	a.status.Disable()
	systray.AddSeparator()

	// 2. Open Dashboard.
	a.openDash = systray.AddMenuItem("Open Dashboard", "Open the Nexus Agent Dashboard window")

	// 3. SSO identity row — hidden until populated.
	a.identity = systray.AddMenuItem("", "")
	a.identity.Disable()
	a.identity.Hide()

	// 4. Pause submenu (hidden when paused) + Resume row (hidden when active).
	a.pauseMenu = systray.AddMenuItem("Pause Protection", "Engage the kill switch")
	a.pause15 = a.pauseMenu.AddSubMenuItem("For 15 minutes", "")
	a.pause1h = a.pauseMenu.AddSubMenuItem("For 1 hour", "")
	a.pause8h = a.pauseMenu.AddSubMenuItem("For 8 hours", "")
	a.pauseInf = a.pauseMenu.AddSubMenuItem("Until I resume", "")
	a.resumeItem = systray.AddMenuItem("Resume Protection", "")
	a.resumeItem.Hide()

	systray.AddSeparator()

	// 5. Settings / About.
	a.settings = systray.AddMenuItem("Settings…", "")
	a.about = systray.AddMenuItem("About Nexus Agent", "")

	systray.AddSeparator()

	// 6. Restart / Quit.
	a.restart = systray.AddMenuItem("Restart Agent", "Restart the agent daemon")
	a.quit = systray.AddMenuItem("Quit Nexus Agent", "")

	// 7. Wire click handlers + start the poll loop.
	a.wireHandlers()
	ctx, cancel := context.WithCancel(context.Background())
	a.pollCancel = cancel
	go a.pollLoop(ctx)
}

func (a *trayApp) onExit() {
	if a.pollCancel != nil {
		a.pollCancel()
	}
}

func (a *trayApp) wireHandlers() {
	go a.relay(a.openDash.ClickedCh, a.handleOpenDashboard)
	go a.relay(a.pause15.ClickedCh, func() { a.handlePause(15 * 60) })
	go a.relay(a.pause1h.ClickedCh, func() { a.handlePause(60 * 60) })
	go a.relay(a.pause8h.ClickedCh, func() { a.handlePause(8 * 60 * 60) })
	go a.relay(a.pauseInf.ClickedCh, func() { a.handlePause(0) })
	go a.relay(a.resumeItem.ClickedCh, a.handleResume)
	go a.relay(a.settings.ClickedCh, a.handleSettings)
	go a.relay(a.about.ClickedCh, a.handleAbout)
	go a.relay(a.restart.ClickedCh, a.handleRestart)
	go a.relay(a.quit.ClickedCh, func() { systray.Quit() })
}

// relay reads click events off the systray's per-item channel and
// dispatches the corresponding handler. Each handler runs in its own
// goroutine so a slow IPC call doesn't queue up subsequent clicks.
func (a *trayApp) relay(ch <-chan struct{}, fn func()) {
	for range ch {
		go fn()
	}
}

func (a *trayApp) pollLoop(ctx context.Context) {
	a.poll(ctx) // immediate first fetch so the menu shows real state quickly
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.poll(ctx)
		}
	}
}

func (a *trayApp) poll(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	snap, err := a.client.GetStatus(ctx)
	if err != nil {
		a.applyDisconnected()
		return
	}
	a.applySnapshot(snap)
}

func (a *trayApp) applyDisconnected() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.applyIcon(stateError)
	a.status.SetTitle("● Agent not running")
	a.lastState = "error"
	a.lastPaused = false
}

func (a *trayApp) applySnapshot(s *trayipc.Snapshot) {
	a.mu.Lock()
	defer a.mu.Unlock()

	pending := s.Agent.DeviceID == ""
	paused := s.Paused

	// Icon + status row.
	state := stateFrom(s.State, paused, pending)
	a.applyIcon(state)
	a.status.SetTitle(statusLabel(state, s.PausedUntil))

	// Pause / Resume swap.
	if paused {
		a.pauseMenu.Hide()
		a.resumeItem.Show()
		a.resumeItem.SetTitle(resumeLabel(s.PausedUntil))
	} else {
		a.pauseMenu.Show()
		a.resumeItem.Hide()
	}

	// SSO identity row — show only when populated and not pending.
	if !pending && s.Agent.SSOEmail != "" {
		a.identity.SetTitle("👤 " + s.Agent.SSOEmail)
		a.identity.Show()
	} else {
		a.identity.Hide()
	}

	// Pending enrollment: the only sensible action is Open Setup
	// (which launches the Dashboard's onboarding page) + Quit.
	// Disable everything else to make that obvious.
	if pending {
		a.openDash.SetTitle("Open Setup…")
		a.pauseMenu.Disable()
		a.resumeItem.Disable()
		a.settings.Disable()
		a.restart.Disable()
	} else {
		a.openDash.SetTitle("Open Dashboard")
		a.pauseMenu.Enable()
		a.resumeItem.Enable()
		a.settings.Enable()
		a.restart.Enable()
	}

	a.lastState = s.State
	a.lastPaused = paused
	a.lastSSO = s.Agent.SSOEmail
}

// Menu actions

func (a *trayApp) handleOpenDashboard() {
	if err := a.launchDashboard(); err != nil {
		a.logger.Warn("launch dashboard failed", "error", err)
	}
}

func (a *trayApp) handlePause(seconds int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.client.PauseProtection(ctx, seconds); err != nil {
		a.logger.Warn("pause failed", "error", err)
		return
	}
	// Eager refresh so the menu doesn't lag a full 2s tick behind
	// the user's click.
	a.poll(ctx)
}

func (a *trayApp) handleResume() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.client.ResumeProtection(ctx); err != nil {
		a.logger.Warn("resume failed", "error", err)
		return
	}
	a.poll(ctx)
}

func (a *trayApp) handleSettings() {
	// Same target as Open Dashboard; the Dashboard owns navigation.
	a.handleOpenDashboard()
}

func (a *trayApp) handleAbout() {
	a.logger.Info("about clicked",
		"version", version,
		"os", runtime.GOOS,
		"note", "open the Dashboard's About page for full info")
	// On Win/Linux there's no obvious system-modal "About" we can
	// pop without bringing in a full GUI toolkit. Treat it as a
	// shortcut to the Dashboard, same as Settings.
	a.handleOpenDashboard()
}

func (a *trayApp) handleRestart() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := a.client.Shutdown(ctx)
	if err != nil {
		a.logger.Warn("restart failed", "error", err)
		return
	}
	if !resp.Acknowledged {
		a.logger.Warn("restart blocked by policy", "error", resp.Error)
	}
}

// launchDashboard searches the standard install locations for the
// Dashboard binary and executes it. The Dashboard runs as a separate
// process; the tray doesn't wait for it.
func (a *trayApp) launchDashboard() error {
	path := a.dashboardPath
	if path == "" {
		path = findDashboard()
	}
	if path == "" {
		return fmt.Errorf("dashboard binary not found (looked in standard install locations)")
	}
	cmd := exec.Command(path)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start dashboard: %w", err)
	}
	// Don't Wait — let the Dashboard outlive this click.
	go func() { _ = cmd.Process.Release() }()
	return nil
}

func findDashboard() string {
	// Candidates in order of preference. Linux: /usr/lib/nexus-agent
	// is the standard package path; the per-user fallback is
	// $HOME/.local/bin so a user-mode install (no root) still
	// works. Windows paths are resolved from PATH and the install
	// directory env var.
	candidates := []string{}
	switch runtime.GOOS {
	case "linux":
		candidates = []string{
			"/usr/lib/nexus-agent/nexus-dashboard",
			"/usr/local/lib/nexus-agent/nexus-dashboard",
		}
		if home, err := os.UserHomeDir(); err == nil {
			candidates = append(candidates, home+"/.local/bin/nexus-dashboard")
		}
	case "windows":
		candidates = []string{
			`C:\Program Files\Nexus Agent\nexus-dashboard.exe`,
			`C:\Program Files (x86)\Nexus Agent\nexus-dashboard.exe`,
		}
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			candidates = append(candidates, local+`\Programs\Nexus Agent\nexus-dashboard.exe`)
		}
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// Last resort: PATH lookup so dev workflows that put the binary
	// on PATH still launch.
	if p, err := exec.LookPath("nexus-dashboard"); err == nil {
		return p
	}
	return ""
}
