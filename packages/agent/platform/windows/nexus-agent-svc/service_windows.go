//go:build windows

// Package winsvc implements the Windows Service handler for the Nexus Agent.
// It runs the agent's main loop under the Windows Service Control Manager so
// the agent starts at boot, restarts on failure, and is managed by `sc` /
// Services MMC.
//
// This package is build-tagged to Windows; on other platforms the binary does
// not include any of this code.
package winsvc

import (
	"context"
	"fmt"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

// AgentRunFunc is the agent's main loop. It must respect ctx cancellation and
// return when ctx is cancelled. The Windows Service handler invokes it inside
// a goroutine when SCM sends a Start.
type AgentRunFunc func(ctx context.Context) error

// nexusAgentService implements svc.Handler.
type nexusAgentService struct {
	run  AgentRunFunc
	elog *eventlog.Log
}

// Execute is the SCM-facing entry point. It transitions the service from
// StartPending → Running, runs the agent loop, and on Stop/Shutdown cancels
// the context and waits for the loop to return.
func (s *nexusAgentService) Execute(args []string, r <-chan svc.ChangeRequest, status chan<- svc.Status) (ssec bool, errno uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() {
		runErr <- s.run(ctx)
	}()

	status <- svc.Status{State: svc.Running, Accepts: accepted}

	agentExitedFirst := false
loop:
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				if s.elog != nil {
					_ = s.elog.Info(1, fmt.Sprintf("Service stop requested (cmd=%v)", c.Cmd))
				}
				break loop
			default:
				if s.elog != nil {
					_ = s.elog.Warning(1, fmt.Sprintf("Unexpected service control: %v", c.Cmd))
				}
			}
		case err := <-runErr:
			// Agent loop exited on its own (e.g. fatal error). Report and exit.
			agentExitedFirst = true
			if s.elog != nil && err != nil {
				_ = s.elog.Error(1, fmt.Sprintf("Agent loop exited unexpectedly: %v", err))
			}
			break loop
		}
	}

	status <- svc.Status{State: svc.StopPending}
	cancel()
	if !agentExitedFirst {
		<-runErr // wait for the loop to acknowledge cancellation
	}
	status <- svc.Status{State: svc.Stopped}
	return false, 0
}

// RunAsService is invoked by `nexus-agent run-svc` (which is in turn invoked
// by SCM after the service is installed). It hands control to svc.Run, which
// blocks until the service stops.
func RunAsService(serviceName string, run AgentRunFunc) error {
	elog, err := eventlog.Open(serviceName)
	if err != nil {
		// Event Log source may not be registered yet on first run; continue
		// without it rather than failing the service start.
		elog = nil
	}
	if elog != nil {
		defer elog.Close() //nolint:errcheck
		_ = elog.Info(1, fmt.Sprintf("Starting %s service", serviceName))
	}

	handler := &nexusAgentService{run: run, elog: elog}
	if err := svc.Run(serviceName, handler); err != nil {
		return fmt.Errorf("svc.Run: %w", err)
	}
	return nil
}
