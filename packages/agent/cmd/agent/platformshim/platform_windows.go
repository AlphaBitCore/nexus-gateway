//go:build windows

package platformshim

import (
	"context"
	"fmt"
	"os"

	winsvc "github.com/AlphaBitCore/nexus-gateway/packages/agent/platform/windows/nexus-agent-svc"
)

const (
	serviceName        = "NexusAgent"
	serviceDisplayName = "Nexus Agent"
	serviceDescription = "AI traffic policy enforcement agent (Nexus Gateway)."
)

// DispatchPlatformCommand handles Windows-only Service Control Manager
// integration commands. Returns true when the command was handled (and the
// program has already exited via os.Exit), false otherwise.
//
// runFn is invoked for the "run-svc" case so that platformshim carries no
// import dependency on package main. Callers pass their agent run function
// (i.e. a wrapper around cmdRun) conforming to the winsvc.AgentRunFunc
// signature: func(ctx context.Context) error.
func DispatchPlatformCommand(cmd string, _ []string, runFn func(context.Context) error) bool {
	switch cmd {
	case "install":
		if err := winsvc.Install(serviceName, serviceDisplayName, serviceDescription); err != nil {
			fmt.Fprintf(os.Stderr, "install failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Service %q installed.\n", serviceName)
		return true

	case "uninstall":
		if err := winsvc.Uninstall(serviceName); err != nil {
			fmt.Fprintf(os.Stderr, "uninstall failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Service %q uninstalled.\n", serviceName)
		return true

	case "run-svc":
		// Invoked by SCM. Hand control to winsvc.RunAsService, which blocks
		// until the service is stopped. The SCM context is propagated to
		// runFn so that SERVICE_CONTROL_STOP triggers graceful agent shutdown.
		if err := winsvc.RunAsService(serviceName, runFn); err != nil {
			fmt.Fprintf(os.Stderr, "run-svc failed: %v\n", err)
			os.Exit(1)
		}
		return true
	}
	return false
}
