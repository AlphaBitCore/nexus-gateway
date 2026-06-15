//go:build linux

package catrust

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSelectTrustCandidate covers the per-distro anchor-directory detection:
// the first candidate whose directory exists wins, and a host with none of
// the known layouts reports not-found. This is the logic that previously
// lived as a Debian-only hardcode in the install-ca shim and left RPM hosts
// with intercepted TLS untrusted.
func TestSelectTrustCandidate(t *testing.T) {
	cands := linuxTrustCandidates()

	cases := []struct {
		name    string
		present map[string]bool // dirs that "exist"
		wantDir string
		wantCmd string // first element of cmd, "" when not found
		wantOK  bool
	}{
		{
			name:    "debian",
			present: map[string]bool{"/usr/local/share/ca-certificates": true},
			wantDir: "/usr/local/share/ca-certificates",
			wantCmd: "update-ca-certificates",
			wantOK:  true,
		},
		{
			name:    "rhel only",
			present: map[string]bool{"/etc/pki/ca-trust/source/anchors": true},
			wantDir: "/etc/pki/ca-trust/source/anchors",
			wantCmd: "update-ca-trust",
			wantOK:  true,
		},
		{
			name:    "arch only",
			present: map[string]bool{"/etc/ca-certificates/trust-source/anchors": true},
			wantDir: "/etc/ca-certificates/trust-source/anchors",
			wantCmd: "update-ca-trust",
			wantOK:  true,
		},
		{
			name: "debian wins over rhel when both present",
			present: map[string]bool{
				"/usr/local/share/ca-certificates": true,
				"/etc/pki/ca-trust/source/anchors": true,
			},
			wantDir: "/usr/local/share/ca-certificates",
			wantCmd: "update-ca-certificates",
			wantOK:  true,
		},
		{
			name:    "none present",
			present: map[string]bool{},
			wantOK:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := selectTrustCandidate(cands, func(p string) bool { return tc.present[p] })
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if got.dir != tc.wantDir {
				t.Errorf("dir = %q, want %q", got.dir, tc.wantDir)
			}
			if len(got.cmd) == 0 || got.cmd[0] != tc.wantCmd {
				t.Errorf("cmd = %v, want first element %q", got.cmd, tc.wantCmd)
			}
		})
	}
}

// TestWriteCAToTrustDir covers the cert placement: written as "<label>.crt"
// in the chosen anchor directory with the exact PEM bytes — verified against
// a temp dir, no root required.
func TestWriteCAToTrustDir(t *testing.T) {
	dir := t.TempDir()
	pem := []byte("-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n")

	path, err := writeCAToTrustDir(dir, "nexus-agent", pem)
	if err != nil {
		t.Fatalf("writeCAToTrustDir: %v", err)
	}
	if want := filepath.Join(dir, "nexus-agent.crt"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(pem) {
		t.Errorf("written content mismatch:\n got %q\nwant %q", got, pem)
	}
}

// TestWriteCAToTrustDir_BadDir pins that a non-existent target directory
// surfaces a wrapped error rather than panicking.
func TestWriteCAToTrustDir_BadDir(t *testing.T) {
	_, err := writeCAToTrustDir(filepath.Join(t.TempDir(), "does-not-exist"), "nexus-agent", []byte("x"))
	if err == nil {
		t.Fatal("expected error writing into a missing directory")
	}
}

// TestDirExists covers the directory probe used by selectTrustCandidate:
// true for a real dir, false for a missing path and for a regular file.
func TestDirExists(t *testing.T) {
	dir := t.TempDir()
	if !dirExists(dir) {
		t.Error("dirExists(tempdir) = false, want true")
	}
	if dirExists(filepath.Join(dir, "nope")) {
		t.Error("dirExists(missing) = true, want false")
	}
	f := filepath.Join(dir, "afile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if dirExists(f) {
		t.Error("dirExists(regular file) = true, want false")
	}
}
