//go:build !darwin

package main

// flushMDNSResponderIfDarwin is a no-op outside macOS. The DNS cache
// behaviour fixed by the Darwin variant is specific to macOS's
// mDNSResponder interaction with NETransparentProxyProvider state
// changes; Linux / Windows DNS stacks don't have the equivalent
// stale-cache race.
func flushMDNSResponderIfDarwin() {}
