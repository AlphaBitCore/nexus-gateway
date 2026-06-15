package core

import (
	"reflect"
	"testing"
)

// --- appendTag dedup --------------------------------------------------------

func TestAppendTag_DedupExistingTag(t *testing.T) {
	in := []string{"severity:public", "rule:a"}
	got := appendTag(in, "rule:a")
	if len(got) != len(in) {
		t.Errorf("dup should be a no-op; got len %d, want %d", len(got), len(in))
	}
	// Returned slice must equal the input.
	if !reflect.DeepEqual(got, in) {
		t.Errorf("dup add should return same content: got %v", got)
	}
}

func TestAppendTag_AppendsNewTag(t *testing.T) {
	in := []string{"a", "b"}
	got := appendTag(in, "c")
	if len(got) != 3 || got[2] != "c" {
		t.Errorf("append: got %v", got)
	}
}

func TestAppendTag_FromNilSlice(t *testing.T) {
	got := appendTag(nil, "first")
	if len(got) != 1 || got[0] != "first" {
		t.Errorf("nil-start append: %v", got)
	}
}

// --- HighestSeverityTag edge --------------------------------------------------

func TestHighestSeverityTag_EmptyInputReturnsEmpty(t *testing.T) {
	if got := HighestSeverityTag(nil); got != "" {
		t.Errorf("nil input: got %q want \"\"", got)
	}
	if got := HighestSeverityTag([]string{}); got != "" {
		t.Errorf("empty slice: got %q", got)
	}
}

func TestHighestSeverityTag_IgnoresUnknownTags(t *testing.T) {
	tags := []string{"random:thing", "severity:internal", "compliance:pii"}
	if got := HighestSeverityTag(tags); got != SeverityInternal {
		t.Errorf("got %q want severity:internal (unknown tags should be ignored)", got)
	}
}

func TestHighestSeverityTag_PicksMaxRanked(t *testing.T) {
	// Restricted (4) wins over confidential (3), internal (2), public (1).
	tags := []string{SeverityPublic, SeverityRestricted, SeverityInternal, SeverityConfidential}
	if got := HighestSeverityTag(tags); got != SeverityRestricted {
		t.Errorf("got %q want severity:restricted", got)
	}
}
