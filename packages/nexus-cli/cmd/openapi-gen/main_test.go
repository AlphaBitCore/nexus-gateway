package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureSrc is the synthetic control-plane fixture shared with the openapigen
// package tests.
func fixtureSrc(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "internal", "openapigen", "testdata", "cp"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRun_Success(t *testing.T) {
	t.Setenv("GOWORK", "off") // load the fixture in module mode
	var out, errb bytes.Buffer
	code := run([]string{"--src", fixtureSrc(t), "--out", t.TempDir(), "--version", "test"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "routes across") {
		t.Errorf("missing summary line: %s", out.String())
	}
	// The fixture contains an unresolved route, so the unresolved section prints.
	if !strings.Contains(out.String(), "unresolved") {
		t.Errorf("expected unresolved section: %s", out.String())
	}
}

// TestRun_RootsFlag covers the --roots flag's parsing (comma split, whitespace
// trim, empty-entry skip) and the multi-root walk end to end: the standalone
// registrar (wired outside RegisterAdminRoutes) is discovered only because it is
// named in --roots, and emits its own kind file.
func TestRun_RootsFlag(t *testing.T) {
	t.Setenv("GOWORK", "off")
	var out, errb bytes.Buffer
	dir := t.TempDir()
	// Embedded whitespace + an empty entry exercise the trim/skip branches.
	code := run([]string{
		"--src", fixtureSrc(t), "--out", dir, "--version", "test",
		"--roots", "RegisterAdminRoutes, ,RegisterStandaloneRoutes",
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "standalone") {
		t.Errorf("expected the standalone kind in the summary: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "standalone.yaml")); err != nil {
		t.Errorf("standalone.yaml not emitted: %v", err)
	}
}

func TestRun_GenerateError(t *testing.T) {
	t.Setenv("GOWORK", "off")
	var out, errb bytes.Buffer
	bad := filepath.Join(t.TempDir(), "does-not-exist")
	code := run([]string{"--src", bad, "--out", t.TempDir()}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit=%d want 1", code)
	}
	if !strings.Contains(errb.String(), "openapi-gen:") {
		t.Errorf("missing error message: %s", errb.String())
	}
}

func TestRun_BadFlag(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"--nope"}, &out, &errb); code != 2 {
		t.Fatalf("exit=%d want 2 for bad flag", code)
	}
}
