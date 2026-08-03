//go:build !windows

package profiling

import (
	"os"
	"syscall"
)

// dumpSignal is the on-demand capture signal. SIGUSR1 is the conventional
// "do something extra" signal on Unix and is not used for anything else by these
// services, so an operator can trigger a profile without disturbing the process.
//
// Split by build tag because Windows has no SIGUSR1: referencing it
// unconditionally is what kept `GOOS=windows` out of the enforced set of the
// agent cross-build gate.
func dumpSignal() (os.Signal, string) { return syscall.SIGUSR1, "SIGUSR1" }
