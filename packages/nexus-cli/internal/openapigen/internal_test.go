package openapigen

import (
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteYAMLError covers the write-failure path: a target whose parent
// directory does not exist.
func TestWriteYAMLError(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "missing-dir", "x.yaml")
	if err := writeYAML(bad, newOMap().Set("k", "v")); err == nil {
		t.Error("expected write error for missing parent directory")
	}
}

// TestLoadTypeErrors covers the fail-loud path: a package that does not
// type-check must surface an error rather than produce a partial spec.
func TestLoadTypeErrors(t *testing.T) {
	src, err := filepath.Abs("testdata/broken")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadControlPlane(src, []string{"./..."}, append(os.Environ(), "GOWORK=off")); err == nil {
		t.Fatal("expected a type-check error from the broken fixture")
	}
}

// TestInfoForOutOfRange covers infoFor returning nil when a position belongs to
// no loaded file.
func TestInfoForOutOfRange(t *testing.T) {
	src, err := filepath.Abs("testdata/cp")
	if err != nil {
		t.Fatal(err)
	}
	l, err := loadControlPlane(src, []string{"./..."}, append(os.Environ(), "GOWORK=off"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if info := l.infoFor(token.NoPos); info != nil {
		t.Error("infoFor(NoPos) should be nil")
	}
	// A render of a nil expression returns an empty string, not a panic.
	if got := l.render(nil); got != "" {
		t.Errorf("render(nil)=%q", got)
	}
}
