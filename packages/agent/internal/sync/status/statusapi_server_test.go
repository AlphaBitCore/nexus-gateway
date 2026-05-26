package status

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	audit "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
)

func newServerTestCollector() *Collector {
	return NewCollector(CollectorConfig{
		Version:         "1.0.0",
		DeviceID:        "dev-test",
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 5 },
	})
}

func dialServer(t *testing.T, socketPath string) net.Conn {
	t.Helper()
	var conn net.Conn
	var err error
	for range 20 {
		conn, err = net.Dial("unix", socketPath)
		if err == nil {
			return conn
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("could not connect to %s: %v", socketPath, err)
	return nil
}

func sendCmd(t *testing.T, conn net.Conn, cmd string) map[string]any {
	t.Helper()
	_, _ = conn.Write([]byte(cmd + "\n"))
	buf := make([]byte, 65536)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		t.Fatalf("unmarshal failed: %v\nraw: %s", err, string(buf[:n]))
	}
	return resp
}

func TestServer_GetStatus(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(socketPath, newServerTestCollector(), nil, nil, nil, nil, nil, nil)
	go func() { _ = srv.Start() }()
	defer srv.Stop()

	conn := dialServer(t, socketPath)
	defer conn.Close() //nolint:errcheck

	resp := sendCmd(t, conn, "GET_STATUS")
	if resp["state"] != "active" {
		t.Errorf("expected active, got %v", resp["state"])
	}
	agent := resp["agent"].(map[string]any)
	if agent["deviceID"] != "dev-test" {
		t.Errorf("expected dev-test, got %v", agent["deviceID"])
	}
}

func TestServer_CheckUpdate(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	checkFn := func() (bool, string, error) { return true, "2.0.0", nil }
	srv := NewServer(socketPath, newServerTestCollector(), checkFn, nil, nil, nil, nil, nil)
	go func() { _ = srv.Start() }()
	defer srv.Stop()

	conn := dialServer(t, socketPath)
	defer conn.Close() //nolint:errcheck

	resp := sendCmd(t, conn, "CHECK_UPDATE")
	if resp["available"] != true {
		t.Errorf("expected available, got %v", resp["available"])
	}
	if resp["version"] != "2.0.0" {
		t.Errorf("expected 2.0.0, got %v", resp["version"])
	}
}

func TestServer_SyncConfig(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	syncFn := func() (bool, string, error) { return true, "v42", nil }
	srv := NewServer(socketPath, newServerTestCollector(), nil, syncFn, nil, nil, nil, nil)
	go func() { _ = srv.Start() }()
	defer srv.Stop()

	conn := dialServer(t, socketPath)
	defer conn.Close() //nolint:errcheck

	resp := sendCmd(t, conn, "SYNC_CONFIG")
	if resp["success"] != true {
		t.Errorf("expected success, got %v", resp["success"])
	}
	if resp["version"] != "v42" {
		t.Errorf("expected v42, got %v", resp["version"])
	}
}

func TestServer_Shutdown(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	shutdownCalled := make(chan struct{}, 1)
	shutdownFn := func() { shutdownCalled <- struct{}{} }
	srv := NewServer(socketPath, newServerTestCollector(), nil, nil, shutdownFn, nil, func() bool { return true }, nil)
	go func() { _ = srv.Start() }()
	defer srv.Stop()

	conn := dialServer(t, socketPath)
	defer conn.Close() //nolint:errcheck

	resp := sendCmd(t, conn, "SHUTDOWN")
	if resp["acknowledged"] != true {
		t.Error("expected acknowledged")
	}
	select {
	case <-shutdownCalled:
	case <-time.After(time.Second):
		t.Error("shutdown was not called within 1s")
	}
}

func TestServer_QueryEvents(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	queryFn := func(search, action string, offset, limit int) ([]audit.Event, int, error) {
		return []audit.Event{{ID: "e1", SourceProcess: "curl", TargetHost: "api.openai.com", Action: "inspect"}}, 1, nil
	}
	srv := NewServer(socketPath, newServerTestCollector(), nil, nil, nil, queryFn, nil, nil)
	go func() { _ = srv.Start() }()
	defer srv.Stop()

	conn := dialServer(t, socketPath)
	defer conn.Close() //nolint:errcheck

	resp := sendCmd(t, conn, "QUERY_EVENTS?q=curl&limit=10")
	if resp["total"] != float64(1) {
		t.Errorf("expected total 1, got %v", resp["total"])
	}
}

func TestServer_UnknownCommand(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(socketPath, newServerTestCollector(), nil, nil, nil, nil, nil, nil)
	go func() { _ = srv.Start() }()
	defer srv.Stop()

	conn := dialServer(t, socketPath)
	defer conn.Close() //nolint:errcheck

	resp := sendCmd(t, conn, "BOGUS_CMD")
	if resp["error"] == nil {
		t.Error("expected error for unknown command")
	}
}
