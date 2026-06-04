package skills

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHTTP scripts a download without network access.
type fakeHTTP struct {
	body   string
	status int
	err    error
}

func (f *fakeHTTP) Get(_ context.Context, _ string) (int, []byte, error) {
	if f.err != nil {
		return 0, nil, f.err
	}
	return f.status, []byte(f.body), nil
}

var _ httpGetter = (*fakeHTTP)(nil)

func TestParseSkillFrontmatter(t *testing.T) {
	src := "---\nname: cost-investigation\ndescription: find the cost driver\nallowed-tools: analyze_cost, navigate\n---\n\n# body\nstep 1\n"
	sk, err := parseSkill([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if sk.Name != "cost-investigation" || sk.Description != "find the cost driver" {
		t.Fatalf("frontmatter parse wrong: %+v", sk)
	}
	if len(sk.AllowedTools) != 2 || sk.AllowedTools[0] != "analyze_cost" || sk.AllowedTools[1] != "navigate" {
		t.Fatalf("allowed-tools must split on comma, got %v", sk.AllowedTools)
	}
	if !strings.Contains(sk.Body, "# body") || strings.Contains(sk.Body, "name:") {
		t.Fatalf("body must be the content after frontmatter, got %q", sk.Body)
	}
}

func TestParseSkillRejectsBadFrontmatter(t *testing.T) {
	cases := map[string]string{
		"no frontmatter": "no frontmatter here",
		"unterminated":   "---\nname: x\ndescription: y",
		"missing name":   "---\ndescription: no name\n---\nbody",
	}
	for name, src := range cases {
		if _, err := parseSkill([]byte(src)); err == nil {
			t.Fatalf("%s must error", name)
		}
	}
}

func TestBuiltinSkillsLoad(t *testing.T) {
	set, err := Load("") // "" => built-ins only, no local dir
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"incident-triage", "cost-investigation", "compliance-audit",
		"node-drift-check", "slo-breach", "provider-outage", "vk-hygiene",
		"cache-effectiveness", "emergency-passthrough",
	} {
		sk, ok := set.Get(want)
		if !ok {
			t.Fatalf("built-in %q must load", want)
		}
		if sk.Body == "" || sk.Description == "" {
			t.Fatalf("built-in %q must carry a body + description, got %+v", want, sk)
		}
	}
	cat := set.Catalog()
	if !strings.Contains(cat, "cost-investigation") || strings.Contains(cat, "# Incident triage") {
		t.Fatalf("catalog lists names+descriptions only (no bodies), got:\n%s", cat)
	}
	// The incident-triage skill narrows tools (progressive disclosure).
	it, _ := set.Get("incident-triage")
	if len(it.AllowedTools) == 0 {
		t.Fatal("incident-triage must declare allowed-tools")
	}
}

func TestLocalDirSkillsAddAlongsideBuiltins(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "deploy-check.md"),
		[]byte("---\nname: deploy-check\ndescription: verify a deploy\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A malformed local file must be skipped, not fatal.
	_ = os.WriteFile(filepath.Join(dir, "broken.md"), []byte("not a skill"), 0o644)
	// A non-.md file is ignored.
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore me"), 0o644)

	set, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set.Get("deploy-check"); !ok {
		t.Fatal("local-dir skill must load")
	}
	if _, ok := set.Get("incident-triage"); !ok {
		t.Fatal("built-ins must still be present alongside local skills")
	}
}

func TestLoadSkillsMissingDirIsFine(t *testing.T) {
	set, err := Load(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("a missing local skill dir must not error, got %v", err)
	}
	if _, ok := set.Get("incident-triage"); !ok {
		t.Fatal("built-ins must still load when the local dir is absent")
	}
}

func TestPreviewThenInstallSkill(t *testing.T) {
	dir := t.TempDir()
	body := "---\nname: net-debug\ndescription: debug networking\n---\nbody\n"
	h := &fakeHTTP{body: body, status: 200}

	// Preview downloads + checksums but writes nothing (review gate).
	info, err := preview(context.Background(), h, "https://example.com/net-debug.md")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "net-debug" || info.Description != "debug networking" || info.SHA256 == "" || string(info.Raw) != body {
		t.Fatalf("preview must parse + checksum + carry raw bytes, got %+v", info)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("preview must not write anything before review")
	}

	// Landing the previewed skill writes the exact reviewed bytes.
	dest, err := InstallFetched(info, dir)
	if err != nil || filepath.Base(dest) != "net-debug.md" {
		t.Fatalf("InstallFetched must land the file, got dest=%q err=%v", dest, err)
	}
	if b, _ := os.ReadFile(dest); string(b) != body {
		t.Fatalf("install must land the reviewed bytes, got %q", string(b))
	}
	// The freshly installed skill loads as a usable skill.
	set, _ := Load(dir)
	if _, ok := set.Get("net-debug"); !ok {
		t.Fatal("an installed skill must be loadable from the local dir")
	}
}

func TestPreviewSkillRejectsHTTPError(t *testing.T) {
	_, err := preview(context.Background(), &fakeHTTP{status: 404}, "https://x/y.md")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("a non-200 download must fail, got %v", err)
	}
}

func TestPreviewSkillRejectsTransportError(t *testing.T) {
	_, err := preview(context.Background(), &fakeHTTP{err: errors.New("dns boom")}, "https://x/y.md")
	if err == nil || !strings.Contains(err.Error(), "dns boom") {
		t.Fatalf("a transport error must propagate, got %v", err)
	}
}

func TestPreviewSkillRejectsUnparseable(t *testing.T) {
	if _, err := preview(context.Background(), &fakeHTTP{body: "garbage", status: 200}, "https://x/y.md"); err == nil {
		t.Fatal("a file that is not a valid skill must be refused at preview")
	}
}

func TestInstallFetchedRejectsEmpty(t *testing.T) {
	if _, err := InstallFetched(InstallInfo{}, t.TempDir()); err == nil {
		t.Fatal("InstallFetched must refuse an empty (un-previewed) info")
	}
}

func TestPreviewThenInstallOverRealHTTP(t *testing.T) {
	body := "---\nname: http-skill\ndescription: served over http\n---\nbody\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	dir := t.TempDir()

	// Preview over real HTTP (exercises newStdHTTP + stdHTTP.Get success) writes nothing.
	pv, err := Preview(context.Background(), srv.URL)
	if err != nil || pv.Name != "http-skill" || pv.SHA256 == "" {
		t.Fatalf("real-HTTP preview must parse + checksum, got %+v err %v", pv, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("preview must not write before review")
	}

	// The reviewed preview lands via the two-step flow (the only install path; an
	// unreviewed one-shot is intentionally absent).
	if _, err := InstallFetched(pv, dir); err != nil {
		t.Fatalf("InstallFetched after a real-HTTP preview must land the file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "http-skill.md")); err != nil {
		t.Fatalf("install must land the file, stat err %v", err)
	}

	// A 404 over real HTTP surfaces as a preview error.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }))
	defer bad.Close()
	if _, err := Preview(context.Background(), bad.URL); err == nil {
		t.Fatal("Preview must fail on a 404 download")
	}
}

func TestInstallFetchedUncreatableDir(t *testing.T) {
	// A localDir whose parent is a regular file cannot be created.
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	info := InstallInfo{Name: "x", Raw: []byte("---\nname: x\ndescription: y\n---\nbody\n")}
	if _, err := InstallFetched(info, filepath.Join(f, "sub")); err == nil {
		t.Fatal("an uncreatable skill dir must error")
	}
}
