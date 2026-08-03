package localfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
)

// statScanLimit bounds how many spilled objects a single Stat examines.
//
// Stat is reachable from an operator-facing introspection call, and a spill root
// is allowed to hold a retention window's worth of bodies — an unbounded
// filepath.Walk over it turns "how much is spilled?" into a stall on the very
// surface an operator reaches for when something is wrong. 50k entries is far
// above any healthy root (the sweeper's size cap bites long before) and costs
// milliseconds; past it the answer is honestly labelled a lower bound.
// A var rather than a const so a test can lower it: proving the bound with 50k
// real files would trade a meaningful assertion for a slow one.
var statScanLimit = 50_000

// errStatLimit / errStatCanceled stop the walk without being reported as
// failures: a bounded or cancelled scan still returns the numbers it gathered.
var (
	errStatLimit    = errors.New("localfs.Stat: scan limit reached")
	errStatCanceled = errors.New("localfs.Stat: context done")
)

// Stat implements SpillStore. It honours ctx and stops at statScanLimit,
// reporting Truncated rather than pretending the partial total is the total.
func (s *Store) Stat(ctx context.Context) (spillstore.Stats, error) {
	stats := spillstore.Stats{Backend: BackendName, ScanLimit: statScanLimit}
	// The ROOT is checked before the walk, and separately from it.
	//
	// The walk callback deliberately swallows per-entry errors (a single
	// unreadable file must not fail the whole measurement), and filepath.Walk
	// hands a root-level lstat failure to that same callback — so a spill root
	// that has been deleted or whose mount has dropped came back as
	// (zero Stats, nil error), which the operator surface renders as "configured
	// and empty". That is the precise confusion this measurement exists to
	// prevent: an operator looking here BECAUSE spilled bodies are unreadable
	// would read that the store is fine and empty.
	if _, rootErr := os.Stat(s.root); rootErr != nil {
		return stats, fmt.Errorf("localfs.Stat: spill root unreadable: %w", rootErr)
	}
	walkErr := filepath.Walk(s.root, func(path string, info os.FileInfo, err error) error {
		// Checked per entry rather than per directory: a single flat day-directory
		// with a million files would otherwise never reach a cancellation point.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errStatCanceled
		}
		if err != nil || info.IsDir() || filepath.Ext(path) != ".bin" {
			return nil //nolint:nilerr // best-effort stat — skip unreadable paths
		}
		if stats.ObjectCount >= int64(statScanLimit) {
			return errStatLimit
		}
		stats.ObjectCount++
		stats.TotalBytes += info.Size()
		mt := info.ModTime()
		if stats.OldestAt.IsZero() || mt.Before(stats.OldestAt) {
			stats.OldestAt = mt
		}
		if mt.After(stats.NewestAt) {
			stats.NewestAt = mt
		}
		return nil
	})
	switch {
	case errors.Is(walkErr, errStatLimit):
		stats.Truncated = true
		return stats, nil
	case errors.Is(walkErr, errStatCanceled):
		// Truncated AND the caller's error, so a timeout on an introspection call
		// is visible as a timeout rather than as a suspiciously small total.
		stats.Truncated = true
		return stats, ctx.Err()
	case walkErr != nil:
		return stats, fmt.Errorf("localfs.Stat: %w", walkErr)
	}
	return stats, nil
}
