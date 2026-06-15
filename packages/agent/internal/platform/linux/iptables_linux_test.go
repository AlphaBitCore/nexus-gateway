//go:build linux

package linux

import "testing"

// TestChainAbsentErr covers the cross-backend "chain does not exist" detection
// that lets the reconciler install its chain on the first tick. The
// iptables-nft "incompatible, use 'nft' tool" case is the critical one: it is
// what real servers (libvirt / docker / kube-proxy populate the nat table with
// nft-native rules) emit when probing the not-yet-created NEXUS_AGENT chain.
// Misclassifying it as fatal disables interception on those hosts.
func TestChainAbsentErr(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   bool
	}{
		{
			name:   "legacy iptables missing chain",
			stderr: "iptables: No chain/target/match by that name.",
			want:   true,
		},
		{
			name:   "iptables-nft incompatible (missing chain on nft-managed nat)",
			stderr: "iptables v1.8.7 (nf_tables): chain `NEXUS_AGENT' in table `nat' is incompatible, use 'nft' tool.",
			want:   true,
		},
		{
			name:   "real failure — permission denied",
			stderr: "iptables v1.8.7 (legacy): can't initialize iptables table `nat': Permission denied (you must be root)",
			want:   false,
		},
		{
			name:   "real failure — kernel module missing",
			stderr: "modprobe: FATAL: Module ip_tables not found; iptables: Table does not exist",
			want:   false,
		},
		{
			name:   "empty stderr",
			stderr: "",
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chainAbsentErr(tc.stderr); got != tc.want {
				t.Errorf("chainAbsentErr(%q) = %v, want %v", tc.stderr, got, tc.want)
			}
		})
	}
}
