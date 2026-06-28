package core

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestObserveContentScan(t *testing.T) {
	const impl = "test-impl-content-scan"

	scanBefore := testutil.ToFloat64(ContentScanTotal.WithLabelValues(impl))
	benignBefore := testutil.ToFloat64(ContentScanBenignTotal.WithLabelValues(impl))

	// A benign scan increments both total and benign.
	ObserveContentScan(impl, 0)
	// A matching scan increments total only.
	ObserveContentScan(impl, 3)

	scanAfter := testutil.ToFloat64(ContentScanTotal.WithLabelValues(impl))
	benignAfter := testutil.ToFloat64(ContentScanBenignTotal.WithLabelValues(impl))

	if got := scanAfter - scanBefore; got != 2 {
		t.Errorf("content_scan_total delta = %v, want 2", got)
	}
	if got := benignAfter - benignBefore; got != 1 {
		t.Errorf("content_scan_benign_total delta = %v, want 1 (only the zero-match scan)", got)
	}
}
