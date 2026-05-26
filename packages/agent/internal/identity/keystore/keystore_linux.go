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
)

// FileStore stores secrets as 0600 files under ~/.nexus/secrets/.
// This is a fallback for Linux where a system keychain may not be available.
type FileStore struct {
	dir string
}

// NewPlatformStore returns a file-backed Store for Linux.
func NewPlatformStore() Store {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".nexus", "secrets")
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
