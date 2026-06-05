package resource

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResourceEmbedMatchesDocsSource guards embed↔docs drift: the embedded spec
// copy must be byte-identical to the generated source under docs/. When a doc spec
// is regenerated, `go generate ./capabilities/resource` must refresh the
// embed. Skips on a build with no repo on disk (the distributed CLI).
func TestResourceEmbedMatchesDocsSource(t *testing.T) {
	const src = "../../../../../docs/users/api/openapi/control-plane"
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Skipf("docs source not present: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		want, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		got, err := resourceSpecFS.ReadFile(resourceSpecDir + "/" + e.Name())
		if err != nil {
			t.Fatalf("docs spec %s is not embedded — run `go generate ./capabilities/resource`", e.Name())
		}
		if !bytes.Equal(want, got) {
			t.Fatalf("embedded %s differs from docs source — run `go generate ./capabilities/resource`", e.Name())
		}
	}
}

func TestResourceCatalogLoadsAndIndexes(t *testing.T) {
	if len(resCatalog.Kinds) == 0 {
		t.Fatal("catalog has no kinds")
	}
	if resCatalog.BasePrefix != "/api/admin" {
		t.Fatalf("basePrefix = %q, want /api/admin", resCatalog.BasePrefix)
	}
	if _, ok := resIdx["virtual-keys"]; !ok {
		t.Fatal("catalog missing the virtual-keys kind")
	}
}

func TestParseCatalog(t *testing.T) {
	cat, idx, err := parseCatalog([]byte("basePrefix: /api/admin\nkinds:\n  - kind: foo\n    file: foo.yaml\n"))
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	if cat.BasePrefix != "/api/admin" || len(idx) != 1 || idx["foo"].File != "foo.yaml" {
		t.Fatalf("parseCatalog mis-parsed: %+v", cat)
	}
	if _, _, err := parseCatalog([]byte("kinds: [this: is, not: valid")); err == nil {
		t.Fatal("parseCatalog must error on malformed YAML")
	}
}

func TestResourceEmbedConsistent(t *testing.T) {
	for _, k := range resCatalog.Kinds {
		if k.File == "" {
			t.Fatalf("kind %q has no spec file in the index", k.Kind)
		}
		if _, err := resourceSpecFS.ReadFile(resourceSpecDir + "/" + k.File); err != nil {
			t.Fatalf("kind %q references %q which is not embedded: %v", k.Kind, k.File, err)
		}
	}
}

// TestKindNames asserts the exported kind-name list is sorted and complete — it is
// what the resource_describe tool shows the model when it names an unknown kind.
func TestKindNames(t *testing.T) {
	names := KindNames()
	if len(names) != len(resCatalog.Kinds) {
		t.Fatalf("KindNames returned %d names, catalog has %d kinds", len(names), len(resCatalog.Kinds))
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("KindNames must be sorted, got %v", names)
		}
	}
	var sawVK bool
	for _, n := range names {
		if n == "virtual-keys" {
			sawVK = true
		}
	}
	if !sawVK {
		t.Fatalf("KindNames must include virtual-keys, got %v", names)
	}
}
