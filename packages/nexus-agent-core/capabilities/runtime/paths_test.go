package runtime

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultMemoryDir(t *testing.T) {
	p, err := DefaultMemoryDir()
	if err != nil {
		t.Fatal(err)
	}
	// The base dir; the store splits it into global/ + <env>/ inside.
	if !strings.HasSuffix(filepath.ToSlash(p), "nexus/memory") {
		t.Fatalf("DefaultMemoryDir must end with nexus/memory, got %q", p)
	}
}

func TestDefaultSessionDir(t *testing.T) {
	dir, err := DefaultSessionDir("local")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(filepath.ToSlash(dir), "nexus/sessions/local") {
		t.Fatalf("DefaultSessionDir must end with nexus/sessions/<env>, got %q", dir)
	}
}
