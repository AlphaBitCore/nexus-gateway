//go:build darwin

package status

import (
	"os"
	"syscall"
	"testing"
)

// TestPeerUIDAllowed encodes the macOS production topology the original
// same-UID check could never express: the daemon runs as root (UID 0) while
// the only legitimate IPC clients — the menu-bar app and the Wails Dashboard —
// run in the console user's GUI session (a non-root UID). The allow predicate
// must admit root and the current console user, and reject any other local
// user that can reach the world-connectable 0666 socket.
func TestPeerUIDAllowed(t *testing.T) {
	const (
		root        = 0
		consoleUser = 501
		otherUser   = 502
	)
	tests := []struct {
		name       string
		peerUID    int
		selfUID    int
		consoleUID int
		want       bool
	}{
		{name: "root daemon talking to itself", peerUID: root, selfUID: root, consoleUID: consoleUser, want: true},
		{name: "console-user GUI to root daemon", peerUID: consoleUser, selfUID: root, consoleUID: consoleUser, want: true},
		{name: "other local user rejected", peerUID: otherUser, selfUID: root, consoleUID: consoleUser, want: false},
		{name: "root allowed even when no console user", peerUID: root, selfUID: root, consoleUID: noConsoleUID, want: true},
		{name: "non-root rejected when console undeterminable", peerUID: consoleUser, selfUID: root, consoleUID: noConsoleUID, want: false},
		{name: "single-user install: self equals console", peerUID: consoleUser, selfUID: consoleUser, consoleUID: consoleUser, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := peerUIDAllowed(tc.peerUID, tc.selfUID, tc.consoleUID); got != tc.want {
				t.Errorf("peerUIDAllowed(%d, %d, %d) = %v, want %v",
					tc.peerUID, tc.selfUID, tc.consoleUID, got, tc.want)
			}
		})
	}
}

// TestConsoleOwnerUID verifies the helper returns the real owner UID of
// /dev/console — the kernel's record of which user holds the GUI session.
// This is the value the allow predicate compares the peer against, so it must
// match a direct stat of the device node rather than a guessed constant.
func TestConsoleOwnerUID(t *testing.T) {
	fi, err := os.Stat("/dev/console")
	if err != nil {
		t.Skipf("/dev/console not statable in this environment: %v", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("/dev/console Sys() is not *syscall.Stat_t")
	}
	want := int(st.Uid)
	if got := consoleOwnerUID(); got != want {
		t.Errorf("consoleOwnerUID() = %d, want %d (owner of /dev/console)", got, want)
	}
}
