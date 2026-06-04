package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// skillTestApp builds an App whose skill dir is a temp dir (no real ~/.config).
func skillTestApp(t *testing.T) *App {
	t.Helper()
	return &App{SkillDir: t.TempDir()}
}

func TestSkillLs_TableAndJSON(t *testing.T) {
	a := skillTestApp(t)
	// A locally installed skill shows up alongside the built-ins.
	if err := os.WriteFile(filepath.Join(a.SkillDir, "deploy-check.md"),
		[]byte("---\nname: deploy-check\ndescription: verify a deploy\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, a, "skill", "ls")
	if err != nil || !strings.Contains(out, "incident-triage") || !strings.Contains(out, "deploy-check") || !strings.Contains(out, "NAME") {
		t.Fatalf("skill ls table wrong: %q err=%v", out, err)
	}

	out, err = runCLI(t, &App{SkillDir: a.SkillDir}, "skill", "ls", "-o", "json")
	if err != nil || !strings.Contains(out, `"name": "incident-triage"`) {
		t.Fatalf("skill ls json wrong: %q err=%v", out, err)
	}
}

func TestSkillInstall_PreviewThenConfirm(t *testing.T) {
	body := "---\nname: net-debug\ndescription: debug networking\n---\nthe playbook body\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	dir := t.TempDir()

	// Without --yes: previews (name + checksum + body) and writes nothing.
	out, err := runCLI(t, &App{SkillDir: dir}, "skill", "install", srv.URL)
	if err != nil {
		t.Fatalf("preview should succeed, err=%v", err)
	}
	if !strings.Contains(out, "net-debug") || !strings.Contains(out, "SHA-256:") || !strings.Contains(out, "the playbook body") || !strings.Contains(out, "--yes") {
		t.Fatalf("preview output must show the skill + checksum + review hint, got %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "net-debug.md")); !os.IsNotExist(err) {
		t.Fatal("preview (no --yes) must not write the skill file")
	}

	// With --yes: lands the file.
	out, err = runCLI(t, &App{SkillDir: dir}, "skill", "install", srv.URL, "--yes")
	if err != nil || !strings.Contains(out, "Installed") {
		t.Fatalf("confirmed install should land the skill, got %q err=%v", out, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "net-debug.md")); string(b) != body {
		t.Fatalf("confirmed install must write the reviewed bytes, got %q", string(b))
	}
}

func TestSkillInstall_RequiresURL(t *testing.T) {
	_, err := runCLI(t, skillTestApp(t), "skill", "install")
	if err == nil || !strings.Contains(err.Error(), "exactly one <url>") {
		t.Fatalf("skill install must require a url, got %v", err)
	}
}

func TestSkillInstall_DownloadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }))
	defer srv.Close()
	_, err := runCLI(t, skillTestApp(t), "skill", "install", srv.URL, "--yes")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("a failed download must error, got %v", err)
	}
}

func TestSkillInstall_LandError(t *testing.T) {
	body := "---\nname: x\ndescription: y\n---\nbody\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	// A skill dir whose parent is a regular file cannot be created.
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runCLI(t, &App{SkillDir: filepath.Join(f, "sub")}, "skill", "install", srv.URL, "--yes")
	if err == nil {
		t.Fatal("a failed landing must surface an error")
	}
}
