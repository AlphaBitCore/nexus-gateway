package executor

import (
	"math/rand"
	"sync"
	"time"

	cfgpolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
)

var (
	backoffRandMu sync.Mutex
	backoffRand   = rand.New(rand.NewSource(time.Now().UnixNano()))
)

// computeBackoff returns the sleep duration before the (tryIdx+1)-th
// attempt on the same target. tryIdx is 1-based: tryIdx=1 means
// "between attempt 1 and attempt 2". Doubles BackoffInitial until
// reaching BackoffMax, then applies uniform ±BackoffJitter jitter.
// Returns 0 (never negative) on extreme jitter.
func computeBackoff(tryIdx int, p cfgpolicy.RetryPolicy) time.Duration {
	if tryIdx < 1 {
		tryIdx = 1
	}
	base := p.BackoffInitial
	for i := 1; i < tryIdx; i++ {
		base *= 2
		if base >= p.BackoffMax {
			base = p.BackoffMax
			break
		}
	}
	if base > p.BackoffMax {
		base = p.BackoffMax
	}
	if p.BackoffJitter > 0 {
		backoffRandMu.Lock()
		delta := float64(base) * p.BackoffJitter
		base += time.Duration(backoffRand.Float64()*2*delta - delta)
		backoffRandMu.Unlock()
	}
	if base < 0 {
		return 0
	}
	return base
}
