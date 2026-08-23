package configkey

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnknownSystemMetadataKeys_NamesTheOnesNobodyClaims.
//
// The production set is the fixture, because the shape of the real table is the
// thing the check has to survive: eleven fixed keys and a dynamic family of
// twelve rows built from a thing type and a config key. A check that enumerated
// the family would fire on all twelve every boot, and a warning that cries wolf
// twelve times a day is one nobody reads — which is the failure this mechanism
// exists to prevent.
func TestUnknownSystemMetadataKeys_NamesTheOnesNobodyClaims(t *testing.T) {
	present := []string{
		// Fixed keys production actually holds.
		"agent.config.version", "agent.settings", "device.auth.mode",
		"hub.spill_upload_secret", "observability.config", "payload_capture.config",
		"siem.config", "streaming_compliance.config",
		// The dynamic family, as production spells it.
		"propagation_ledger:agent:hooks",
		"propagation_ledger:ai-gateway:credentials",
		"propagation_ledger:ai-gateway:routing_rules",
		"propagation_ledger:compliance-proxy:streaming_compliance",
		// The row this whole mechanism exists for: read by nothing, its value
		// the OPPOSITE of the setting in force, its timestamp still moving.
		"audit.payload",
		// Two more of the same shape, retired from the code at the same time.
		"alerting.config", "proxy.killswitch",
	}

	got := UnknownSystemMetadataKeys(present)
	want := map[string]bool{"audit.payload": true, "alerting.config": true, "proxy.killswitch": true}

	for _, k := range got {
		if !want[k] {
			t.Errorf("%q reported as unknown, but it is claimed — a warning that names live keys "+
				"is one an operator learns to ignore", k)
		}
		delete(want, k)
	}
	for k := range want {
		t.Errorf("%q was NOT reported; it is exactly the shape this check exists for — read by "+
			"nothing, and answering the obvious question with a stale value", k)
	}
}

// TestSystemMetadataKeys_EveryEntryIsClaimedBySomeCode is the reverse check, and
// the one that keeps the list from becoming its own kind of stale.
//
// An entry here is a claim that some code path reads or writes that key.
// Deleting the code and leaving the entry re-creates the original problem one
// level up: the check would then vouch for a key nobody uses, and the next
// person to ask "who reads this?" would get a wrong answer from the registry
// instead of from the table.
func TestSystemMetadataKeys_EveryEntryIsClaimedBySomeCode(t *testing.T) {
	root := repoRootFromHere(t)
	var sources []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		// A subtree we could not enter is a subtree we did not search. This
		// sweep's conclusion is "no file mentions this key", so an unsearched
		// corner turns a miss into a false accusation.
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if name == "node_modules" || name == ".git" || name == "dist" || name == "worktrees" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			sources = append(sources, path)
		}
		return nil
	})
	if err != nil || len(sources) == 0 {
		t.Fatalf("walked %d source files (err=%v) — the sweep found nothing, so it would vouch "+
			"for every entry no matter what the code said", len(sources), err)
	}

	blob := make([]string, 0, len(sources))
	for _, p := range sources {
		b, readErr := os.ReadFile(p)
		if readErr != nil {
			// Same reason the walk error is fatal: a file we skipped is a file
			// we did not search, and the verdict below is about absence.
			t.Fatalf("read %s: %v", p, readErr)
		}
		blob = append(blob, string(b))
	}
	all := strings.Join(blob, "\n")

	for _, key := range SystemMetadataKeys {
		if !strings.Contains(all, `"`+key+`"`) {
			t.Errorf("%q is registered but no non-test Go file mentions it. Either the code that "+
				"used it was deleted — in which case the row is now exactly the kind this check "+
				"is meant to surface, and the entry is hiding it — or it moved and the entry "+
				"needs updating", key)
		}
	}
}

// repoRootFromHere walks up until it finds go.work, so the sweep covers every
// package rather than this one.
func repoRootFromHere(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for range 10 {
		if _, statErr := os.Stat(filepath.Join(dir, "go.work")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("go.work not found walking up; the sweep cannot find the repo root and would " +
		"silently check nothing")
	return ""
}
