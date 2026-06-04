package views

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
)

// TestConstructors verifies every exported constructor (the shell's entry points)
// builds a usable kit.ViewModel and survives the Init→Update→View first-frame cycle
// without panicking — exercising each view's initial fetch + render path.
func TestConstructors(t *testing.T) {
	gw, s := sampleGateway(), testSession()
	views := []kit.ViewModel{
		NewCockpit(gw), NewRadar(gw), NewAlerts(gw), NewNodes(gw),
		NewCompliance(gw), NewJobs(gw), NewConfigSync(gw), NewModels(gw),
		NewEvent(gw, s), NewChat(gw, s), NewLab(gw, s), NewKill(gw, s),
		NewSLO(gw, s), NewCost(gw, s), NewVKs(gw, s), NewRouting(gw, s),
	}
	for i, v := range views {
		if v == nil {
			t.Fatalf("constructor %d returned nil", i)
		}
		if cmd := v.Init(); cmd != nil {
			if msg := cmd(); msg != nil {
				v, _ = v.Update(msg) // process the initial fetch result
			}
		}
		_ = v.View(100, 30) // must render without panicking
	}
}
