//go:build windows

package profiling

import "os"

// dumpSignal reports that Windows has no on-demand capture signal.
//
// SIGUSR1 does not exist there, and the plausible substitutes are worse than
// nothing: SIGBREAK is what Ctrl-Break delivers and SIGINT/SIGTERM already mean
// shutdown, so binding a profile capture to any of them would make a routine
// operator keystroke dump profiles — or, worse, make a shutdown signal look
// handled. Live profiling on Windows goes through NEXUS_PPROF_ADDR instead,
// which is platform-neutral and already supported.
func dumpSignal() (os.Signal, string) { return nil, "" }
