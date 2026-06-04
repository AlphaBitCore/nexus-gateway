//go:build windows

package platformshim

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

// CmdInstallWfpCheck is the MSI custom-action target invoked after
// the `NexusWFP` kernel-mode service is registered + started
// (FR-W4.1, E59 replacement of E42's CmdInstallWinDivertCheck).
// Runs as the installing user (Impersonate="yes" on the WiX
// <CustomAction>), which lets it pop a MessageBox into the user's
// desktop session — deferred system-context actions cannot.
//
// Behaviour mirrors the WinDivert-era helper (A1-spec Q6 + the
// later "OK/Cancel" refinement) but checks the NexusWFP service
// instead:
//
//  1. If NexusWFP is RUNNING → exit 0 silently; MSI continues.
//
//  2. If NexusWFP is missing or not RUNNING (Secure-Boot policy
//     rejected our signature, third-party AV blocked driver load,
//     etc.) → show a modal MessageBoxW with OK / Cancel buttons.
//     - User picks OK   → exit 0; MSI completes with the agent in
//     SystemProxyFallback mode.
//     - User picks Cancel → exit 1; MSI rolls back the install
//     per the <CustomAction Return="check"/>
//     contract.
func CmdInstallWfpCheck(_ []string) int {
	running, queryErr := isNexusWfpRunning()
	if running {
		return 0
	}

	reason := "NexusWFP service is not in the RUNNING state."
	if queryErr != nil {
		reason = queryErr.Error()
	}

	const (
		mbOkCancel    uintptr = 0x00000001
		mbIconWarning uintptr = 0x00000030
		mbTopMost     uintptr = 0x00040000
		idOk          uintptr = 1
		idCancel      uintptr = 2
	)
	title := windows.StringToUTF16Ptr("Nexus Agent")
	body := windows.StringToUTF16Ptr(
		"NexusWFP kernel driver could not be loaded.\n\n" +
			"Some apps that ignore the system proxy may bypass the Nexus Agent's " +
			"traffic filtering. The agent will still work, but coverage will be partial.\n\n" +
			"Contact support@nexus-gateway.com for help with driver signing or " +
			"Anti-Virus exemption.\n\n" +
			"Click OK to continue install with limited filtering, or Cancel to abort.\n\n" +
			"Details: " + reason)

	user32 := windows.NewLazySystemDLL("user32.dll")
	procMessageBoxW := user32.NewProc("MessageBoxW")
	ret, _, _ := procMessageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(body)),
		uintptr(unsafe.Pointer(title)),
		mbOkCancel|mbIconWarning|mbTopMost,
	)
	switch ret {
	case idOk:
		return 0
	case idCancel:
		fmt.Fprintln(os.Stderr, "user cancelled install after NexusWFP load failure")
		return 1
	default:
		fmt.Fprintf(os.Stderr, "MessageBoxW returned unexpected %d; continuing install\n", ret)
		return 0
	}
}

// isNexusWfpRunning returns true when the `NexusWFP` service exists
// and is in the SERVICE_RUNNING state. Any failure to query SCM is
// treated as "not running" so we err on the side of warning the user.
func isNexusWfpRunning() (bool, error) {
	m, err := mgr.Connect()
	if err != nil {
		return false, fmt.Errorf("OpenSCManager: %w", err)
	}
	defer m.Disconnect() //nolint:errcheck

	s, err := m.OpenService("NexusWFP")
	if err != nil {
		return false, fmt.Errorf("OpenService NexusWFP: %w", err)
	}
	defer s.Close() //nolint:errcheck

	status, err := s.Query()
	if err != nil {
		return false, fmt.Errorf("QueryServiceStatus: %w", err)
	}
	if status.State == windows.SERVICE_RUNNING {
		return true, nil
	}
	return false, fmt.Errorf("NexusWFP state is %d (want SERVICE_RUNNING=%d)",
		status.State, windows.SERVICE_RUNNING)
}
