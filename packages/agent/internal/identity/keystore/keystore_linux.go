//go:build linux

// LIMITATION: File-based secret storage with 0600 permissions.
// Unlike macOS Keychain or Windows DPAPI, provides only filesystem ACL
// protection. Any process running as the same UID can read without
// user interaction. For production, consider TPM2 or a secrets manager.

package keystore

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/paths"
)

// FileStore stores secrets as 0600 files under ~/.nexus/secrets/.
// This is a fallback for Linux where a system keychain may not be available.
type FileStore struct {
	dir string
}

// secretsDir resolves the directory holding this host's at-rest secrets: the
// SQLCipher key for the audit queue and the Ed25519 attestation key. It is the
// at-rest encryption root, so where it lands is not a detail.
//
// Discarding os.UserHomeDir()'s error is not safe here. When it fails home is "", and
// filepath.Join("", ".nexus", "secrets") is a RELATIVE path resolved against
// the process working directory. On Windows that is not hypothetical: the agent
// runs as a LocalSystem service with no user profile, the lookup fails every
// time, and the keys land under whatever the service's CWD happens to be
// (typically C:\Windows\System32) with nothing logged.
//
// The fallback is the machine-scoped StateDir the paths package already owns
// (%ProgramData%\NexusAgent on Windows, /var/lib/nexus-agent on Linux), which
// is absolute on every platform and correct for a service. The HAPPY path is
// deliberately unchanged: relocating a keystore that already holds keys would
// lock an existing agent out of its own encrypted audit DB.
func secretsDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		dir := filepath.Join(paths.DefaultPaths().StateDir, "secrets")
		slog.Warn("no home directory for the at-rest key store; using the machine state dir",
			"path", dir, "error", err)
		return dir
	}
	return filepath.Join(home, ".nexus", "secrets")
}

// NewPlatformStore returns a file-backed Store for Linux.
func NewPlatformStore() Store {
	dir := secretsDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		slog.Warn("cannot create secrets directory", "path", dir, "error", err)
	}
	return &FileStore{dir: dir}
}

func (s *FileStore) Get(key string) ([]byte, error) {
	path := filepath.Join(s.dir, key+".key")
	encoded, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read secret %s: %w", key, err)
	}
	return base64.StdEncoding.DecodeString(string(encoded))
}

func (s *FileStore) Set(key string, value []byte) error {
	path := filepath.Join(s.dir, key+".key")
	encoded := base64.StdEncoding.EncodeToString(value)
	return os.WriteFile(path, []byte(encoded), 0600)
}

func (s *FileStore) Delete(key string) error {
	path := filepath.Join(s.dir, key+".key")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
