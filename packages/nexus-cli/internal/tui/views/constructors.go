// Package views holds the operator console's dashboard views — the cockpit and the
// per-domain presenters. Each is a thin presenter over a kit.Gateway; the shell
// holds them through kit.ViewModel + optional behavioral seams, so concrete view
// types stay package-private. Three views the agent drives directly (Models, Event,
// Radar) are exported so the shell can type-assert and call their drive methods.
package views

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
)

func NewCockpit(gw kit.Gateway) kit.ViewModel    { return newCockpit(gw) }
func NewRadar(gw kit.Gateway) kit.ViewModel      { return newRadar(gw) }
func NewAlerts(gw kit.Gateway) kit.ViewModel     { return newAlerts(gw) }
func NewNodes(gw kit.Gateway) kit.ViewModel      { return newNodes(gw) }
func NewCompliance(gw kit.Gateway) kit.ViewModel { return newCompliance(gw) }
func NewJobs(gw kit.Gateway) kit.ViewModel       { return newJobs(gw) }
func NewConfigSync(gw kit.Gateway) kit.ViewModel { return newConfigSync(gw) }
func NewModels(gw kit.Gateway) kit.ViewModel     { return newModels(gw) }

func NewEvent(gw kit.Gateway, s kit.Session) kit.ViewModel   { return newEvent(gw, s) }
func NewChat(gw kit.Gateway, s kit.Session) kit.ViewModel    { return newChat(gw, s) }
func NewLab(gw kit.Gateway, s kit.Session) kit.ViewModel     { return newLab(gw, s) }
func NewKill(gw kit.Gateway, s kit.Session) kit.ViewModel    { return newKill(gw, s) }
func NewSLO(gw kit.Gateway, s kit.Session) kit.ViewModel     { return newSLO(gw, s) }
func NewCost(gw kit.Gateway, s kit.Session) kit.ViewModel    { return newCost(gw, s) }
func NewVKs(gw kit.Gateway, s kit.Session) kit.ViewModel     { return newVKs(gw, s) }
func NewRouting(gw kit.Gateway, s kit.Session) kit.ViewModel { return newRouting(gw, s) }
