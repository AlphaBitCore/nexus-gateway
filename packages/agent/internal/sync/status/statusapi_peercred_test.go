package status

import (
	"github.com/goccy/go-json"
	"net"
	"testing"
	"time"
)

// TestCheckPeerUID_NetPipe verifies that checkPeerUID is a no-op for
// connections that are not *net.UnixConn (e.g. net.Pipe used in tests).
// This is the standard test harness path — it must not be rejected.
func TestCheckPeerUID_NetPipe(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	if err := checkPeerUID(c1); err != nil {
		t.Errorf("checkPeerUID on net.Pipe should return nil, got: %v", err)
	}
}

// TestCheckPeerUID_SameUID verifies that a connection from the same process
// (same UID) passes the peer check. This is the normal operating case:
// the test process connects to a Unix socket created by the same test process,
// so the peer UID equals os.Getuid().
func TestCheckPeerUID_SameUID(t *testing.T) {
	socketPath := testEndpoint(t)

	ln, err := platformListen(socketPath)
	if err != nil {
		t.Fatalf("platformListen: %v", err)
	}
	defer ln.Close() //nolint:errcheck

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- c
	}()

	client, err := dialEndpoint(socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close() //nolint:errcheck

	select {
	case server := <-accepted:
		defer server.Close() //nolint:errcheck
		if err := checkPeerUID(server); err != nil {
			t.Errorf("checkPeerUID same-UID connection should succeed, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not accept connection within 2s")
	}
}

// TestHandleConn_PeerCheckGates verifies that handleConn only dispatches
// commands after checkPeerUID passes. Since checkPeerUID always passes for
// same-UID connections (the only kind a self-test can produce), this test
// confirms the normal path: connection from same UID receives a valid response.
func TestHandleConn_PeerCheckGates(t *testing.T) {
	socketPath := testEndpoint(t)
	srv := NewServer(socketPath, newTestCollector(), nil, nil, nil, nil, nil, nil)
	go func() { _ = srv.Start() }()
	defer srv.Stop()

	conn := dialServer(t, socketPath)
	defer conn.Close() //nolint:errcheck

	// If the peer check rejected us, the connection would close immediately
	// and GET_STATUS would time out or return an error. A valid JSON response
	// with a "state" key confirms the command made it through handleConn.
	_, err := conn.Write([]byte("GET_STATUS\n"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4096)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("read failed after peer check should have passed: n=%d err=%v", n, err)
	}
	var resp map[string]any
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		t.Fatalf("response not valid JSON: %v (raw: %s)", err, string(buf[:n]))
	}
	if resp["state"] == nil {
		t.Errorf("expected state in GET_STATUS response, got: %v", resp)
	}
}
