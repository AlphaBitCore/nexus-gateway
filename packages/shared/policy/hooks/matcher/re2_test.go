package matcher

import (
	"reflect"
	"testing"
)

func TestCompileRE2_SkipsBadPatternKeepsRest(t *testing.T) {
	m, bad := CompileRE2([]Pattern{
		{ID: 0, Expr: `sk-[A-Za-z0-9]{10,}`},
		{ID: 1, Expr: `(`}, // unterminated group → uncompilable
		{ID: 2, Expr: `AKIA`, Flags: "i"},
	})
	if len(bad) != 1 || bad[0].ID != 1 {
		t.Fatalf("bad = %+v, want exactly the ID-1 pattern", bad)
	}
	// The two good patterns still scan.
	hits := m.Scan([]string{"key sk-ABCDEFGHIJ and akia"}, true)
	got := map[int]bool{}
	for _, h := range hits {
		got[h.ID] = true
	}
	if !got[0] || !got[2] {
		t.Fatalf("expected patterns 0 and 2 to fire, got hits %+v", hits)
	}
	if got[1] {
		t.Fatalf("uncompilable pattern 1 must not fire")
	}
}

func TestScan_FirstOnly_OneHitPerSegment(t *testing.T) {
	m, _ := CompileRE2([]Pattern{{ID: 7, Expr: `\d{3}`}})
	hits := m.Scan([]string{"123 456 789"}, true)
	if len(hits) != 1 {
		t.Fatalf("firstOnly should yield 1 hit, got %d: %+v", len(hits), hits)
	}
	if hits[0] != (Hit{ID: 7, Seg: 0, Start: 0, End: 3}) {
		t.Fatalf("first hit = %+v, want span of leading 123", hits[0])
	}
}

func TestScan_AllSpans_ForRedaction(t *testing.T) {
	m, _ := CompileRE2([]Pattern{{ID: 7, Expr: `\d{3}`}})
	hits := m.Scan([]string{"123 456 789"}, false)
	want := []Hit{
		{ID: 7, Seg: 0, Start: 0, End: 3},
		{ID: 7, Seg: 0, Start: 4, End: 7},
		{ID: 7, Seg: 0, Start: 8, End: 11},
	}
	if !reflect.DeepEqual(hits, want) {
		t.Fatalf("all-spans = %+v, want every 3-digit run %+v", hits, want)
	}
}

func TestScan_CaseInsensitiveFlag(t *testing.T) {
	m, _ := CompileRE2([]Pattern{{ID: 1, Expr: `bearer`, Flags: "i"}})
	if h := m.Scan([]string{"Authorization: BEARER xyz"}, true); len(h) != 1 {
		t.Fatalf("(?i) flag should match BEARER, got %+v", h)
	}
	m2, _ := CompileRE2([]Pattern{{ID: 1, Expr: `bearer`}})
	if h := m2.Scan([]string{"Authorization: BEARER xyz"}, true); len(h) != 0 {
		t.Fatalf("case-sensitive must not match BEARER, got %+v", h)
	}
}

func TestScan_MultiSegment_ReportsSegmentIndex(t *testing.T) {
	m, _ := CompileRE2([]Pattern{{ID: 5, Expr: `AKIA[0-9A-Z]{16}`}})
	hits := m.Scan([]string{"benign first", "creds AKIA1234567890ABCDEF here"}, true)
	if len(hits) != 1 || hits[0].Seg != 1 {
		t.Fatalf("hit should be in segment 1, got %+v", hits)
	}
}

func TestScan_NoSegmentsOrNoMatch_Empty(t *testing.T) {
	m, _ := CompileRE2([]Pattern{{ID: 1, Expr: `secret`}})
	if h := m.Scan(nil, true); len(h) != 0 {
		t.Fatalf("nil segments → no hits, got %+v", h)
	}
	if h := m.Scan([]string{"perfectly benign text"}, false); len(h) != 0 {
		t.Fatalf("no match → no hits, got %+v", h)
	}
}
