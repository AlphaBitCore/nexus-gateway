//go:build windows

package winsvc

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// Install registers the agent binary as a Windows Service via SCM.
// The service is configured for auto-start, runs as LocalSystem, and is set
// to restart automatically on crash.
func Install(serviceName, displayName, description string) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to SCM: %w", err)
	}
	defer m.Disconnect()

	// Idempotent: if it already exists, return early
	if s, err := m.OpenService(serviceName); err == nil {
		s.Close()
		return fmt.Errorf("service %q already installed", serviceName)
	}

	cfg := mgr.Config{
		ServiceType:    windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:      mgr.StartAutomatic,
		ErrorControl:   mgr.ErrorNormal,
		BinaryPathName: exePath,
		DisplayName:    displayName,
		Description:    description,
	}
	// Run as LocalSystem (default when ServiceStartName is empty)
	cfg.ServiceStartName = ""

	s, err := m.CreateService(serviceName, exePath, cfg, "run-svc")
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()

	// Configure recovery actions: restart after 5s on first two failures, then again after 30s
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}, 86400); err != nil {
		// Best-effort; don't undo the install
		fmt.Fprintf(os.Stderr, "warning: failed to set recovery actions: %v\n", err)
	}

	// Register Event Log source so the service can write to the Application log
	if err := eventlog.InstallAsEventCreate(serviceName, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		// Already exists is fine
		fmt.Fprintf(os.Stderr, "warning: eventlog install: %v\n", err)
	}

	return nil
}

// Uninstall stops the service (if running) and removes it from SCM.
func Uninstall(serviceName string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to SCM: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %q not installed", serviceName)
	}
	defer s.Close()

	// Best-effort stop before delete
	status, _ := s.Query()
	if status.State != svc.Stopped {
		_, _ = s.Control(svc.Stop)
		// Wait up to 30s for stop
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			status, _ = s.Query()
			if status.State == svc.Stopped {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}

	// Remove Event Log source (best-effort)
	_ = eventlog.Remove(serviceName)
	return nil
}

// Start asks SCM to start the service.
func Start(serviceName string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Start()
}

// Stop asks SCM to stop the service and waits up to 30s.
func Stop(serviceName string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	defer s.Close()
	if _, err := s.Control(svc.Stop); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		status, _ := s.Query()
		if status.State == svc.Stopped {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("service %q did not stop within 30s", serviceName)
}
