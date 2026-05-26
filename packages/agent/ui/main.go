// Command nexus-dashboard is the end-user desktop dashboard for the
// Nexus Agent desktop dashboard. It launches a native WebView window
// hosting a React UI that talks to the local agent daemon over the
// existing statusapi Unix socket — no outbound HTTP. The window is
// launched on demand by the menu bar and exits when closed.
//
// The Go shell is intentionally small: a single AgentBridge struct
// (bridge.go) exposes IPC commands to JavaScript via Wails bindings,
// and the frontend (built into ./frontend/dist by Vite) does the
// rendering.
package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	bridge := NewAgentBridge()
	err := wails.Run(&options.App{
		Title:             "Nexus Agent",
		Width:             1000,
		Height:            700,
		MinWidth:          800,
		MinHeight:         600,
		HideWindowOnClose: false, // explicit on-demand lifecycle: closing the window terminates the process
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 245, G: 246, B: 248, A: 1},
		OnStartup:        bridge.onStartup,
		OnShutdown:       bridge.onShutdown,
		Bind: []interface{}{
			bridge,
		},
		Mac: &mac.Options{
			TitleBar: &mac.TitleBar{
				TitlebarAppearsTransparent: false,
				HideTitle:                  false,
				HideTitleBar:               false,
				FullSizeContent:            false,
				UseToolbar:                 false,
			},
			Appearance:           mac.NSAppearanceNameAqua,
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			About: &mac.AboutInfo{
				Title:   "Nexus Agent Dashboard",
				Message: "End-user dashboard for the Nexus Agent.\n© Nexus Gateway.",
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
