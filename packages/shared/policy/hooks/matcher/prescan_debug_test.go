//go:build vectorscan

package matcher

import (
	"fmt"
	"testing"
)

// Load-bearing beyond this package: docker/services/Dockerfile and
// scripts/release/build-tarball.sh name this test explicitly and assert that
// exactly three named tests pass, as the proof that the libhs a release links
// actually scans. Renaming or folding it into a subtest breaks the image build.
func TestHSSelfTest(t *testing.T) {
	ver, scanRC, matches := HSSelfTest()
	fmt.Printf("HS_SELFTEST version=%s scan_rc=%d matches=%d\n", ver, scanRC, matches)
	fmt.Printf("(rc legend: 0=SUCCESS -3=SCAN_TERMINATED -4=DB_VERSION -5=DB_PLATFORM -6=DB_MODE -7=BAD_ALIGN -10=ARCH_ERROR)\n")
	if matches != 1 {
		t.Errorf("libhs NON-FUNCTIONAL: /secret/ on \"a secret here\" → %d matches (scan_rc=%d) — content detection silently fails", matches, scanRC)
	}
}
