//go:build linux || windows

package main

import (
	_ "embed"
	"fmt"
	"time"

	"fyne.io/systray"
)

type uiState int

const (
	stateActive uiState = iota
	stateDegraded
	stateError
	statePaused
	statePendingEnrollment
)

// Icon assets. Each is a single-colour SVG-rasterised PNG sized for
// the system tray (~16-22 px on Linux, 16 px on Windows). Held in
// the binary via go:embed so the tray installs with zero loose
// resource files.
//
// All four glyphs are 32x32 PNGs designed to remain legible at the
// system tray's downscaling.
//
//go:embed icons/active.png
var iconActive []byte

//go:embed icons/degraded.png
var iconDegraded []byte

//go:embed icons/error.png
var iconError []byte

//go:embed icons/paused.png
var iconPaused []byte

func stateFrom(rawState string, paused, pending bool) uiState {
	if pending {
		return statePendingEnrollment
	}
	if paused {
		return statePaused
	}
	switch rawState {
	case "active":
		return stateActive
	case "degraded":
		return stateDegraded
	default:
		return stateError
	}
}

func (a *trayApp) applyIcon(s uiState) {
	var bytes []byte
	switch s {
	case stateActive:
		bytes = iconActive
	case stateDegraded, statePaused:
		// Reuse the yellow glyph for both — the menu's status row
		// disambiguates "needs attention" from "you paused this".
		if s == statePaused {
			bytes = iconPaused
		} else {
			bytes = iconDegraded
		}
	default:
		bytes = iconError
	}
	if len(bytes) > 0 {
		systray.SetIcon(bytes)
	}
}

func statusLabel(s uiState, pausedUntil string) string {
	switch s {
	case stateActive:
		return "● Protection Active"
	case stateDegraded:
		return "● Attention Needed"
	case statePaused:
		if t := formatResumesAt(pausedUntil); t != "" {
			return "● Protection Paused — resumes at " + t
		}
		return "● Protection Paused"
	case statePendingEnrollment:
		return "● Setup Required"
	default:
		return "● Protection Stopped"
	}
}

func resumeLabel(pausedUntil string) string {
	if t := formatResumesAt(pausedUntil); t != "" {
		return fmt.Sprintf("Resume Protection (%s)", t)
	}
	return "Resume Protection"
}

func formatResumesAt(rfc3339 string) string {
	if rfc3339 == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return ""
	}
	return t.Local().Format("15:04")
}
