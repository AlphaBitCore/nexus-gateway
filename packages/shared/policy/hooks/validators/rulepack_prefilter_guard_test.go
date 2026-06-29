package validators

// CI guard: every regex shipped in a seed rule-pack must be friendly to the
// ai-gateway raw-body prefilter (the perf path that skips structured extraction on
// benign traffic). A hostile pattern does not break correctness but silently
// degrades that fast path (collapses the hook's prescan, or matches everything →
// no speedup + false positives). Catching it here keeps the seed corpus fast and
// is the CI consumer of matcher.DiagnosePrefilter — the same diagnosis a future
// rule-authoring UI surfaces at edit time. If this fails, fix the pattern (drop
// leading/trailing wildcards / fully-optional quantifiers) or, if the broad match
// is intentional, exempt it explicitly here with a recorded reason.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"
)

func repoRootFromCaller(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// this file: <root>/packages/shared/policy/hooks/validators/<file> → up 5.
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../../../.."))
}

func TestSeedRulePacks_PrefilterFriendly(t *testing.T) {
	dir := filepath.Join(repoRootFromCaller(t), "tools/db-migrate/seed/rule-packs")
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no rule-pack yaml found under %s (err=%v)", dir, err)
	}
	sort.Strings(files)

	reSingle := regexp.MustCompile(`(?m)^\s*pattern:\s*'(.*)'\s*$`)
	reDouble := regexp.MustCompile(`(?m)^\s*pattern:\s*"(.*)"\s*$`)
	reID := regexp.MustCompile(`(?m)^\s*-\s*id:\s*(\S+)`)

	scanned, hostile := 0, 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		s := string(b)
		ids := reID.FindAllStringSubmatch(s, -1)
		pats := append(reSingle.FindAllStringSubmatch(s, -1), reDouble.FindAllStringSubmatch(s, -1)...)
		for i, m := range pats {
			scanned++
			d := matcher.DiagnosePrefilter(m[1])
			if !d.Friendly {
				hostile++
				id := "?"
				if i < len(ids) {
					id = ids[i][1]
				}
				t.Errorf("%s rule %s prefilter-hostile [%s]: %s\n  pattern: %s",
					filepath.Base(f), id, d.Code, d.Message, m[1])
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned 0 patterns — the pattern-extraction regex likely drifted from the yaml format")
	}
	t.Logf("scanned %d seed rule-pack patterns, %d hostile", scanned, hostile)
}
