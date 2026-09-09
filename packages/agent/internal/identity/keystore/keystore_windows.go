//go:build windows

package keystore

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/paths"
)

var (
	crypt32         = syscall.NewLazyDLL("crypt32.dll")
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	procEncryptData = crypt32.NewProc("CryptProtectData")
	procDecryptData = crypt32.NewProc("CryptUnprotectData")
	procLocalFree   = kernel32.NewProc("LocalFree")
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

// DPAPIStore uses Windows DPAPI for user-bound secret storage.
type DPAPIStore struct {
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

// NewPlatformStore returns a Windows DPAPI-backed Store.
func NewPlatformStore() Store {
	dir := secretsDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		slog.Warn("cannot create secrets directory", "path", dir, "error", err)
	}
	return &DPAPIStore{dir: dir}
}

func (s *DPAPIStore) Get(key string) ([]byte, error) {
	path := filepath.Join(s.dir, key+".dpapi")
	encoded, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	encrypted, err := base64.StdEncoding.DecodeString(string(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode stored secret: %w", err)
	}
	return dpUnprotect(encrypted)
}

func (s *DPAPIStore) Set(key string, value []byte) error {
	encrypted, err := dpProtect(value)
	if err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString(encrypted)
	path := filepath.Join(s.dir, key+".dpapi")
	return os.WriteFile(path, []byte(encoded), 0600)
}

func (s *DPAPIStore) Delete(key string) error {
	path := filepath.Join(s.dir, key+".dpapi")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func dpProtect(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("CryptProtectData: empty input")
	}
	in := dataBlob{cbData: uint32(len(data)), pbData: &data[0]}
	var out dataBlob
	r, _, err := procEncryptData.Call(
		uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return nil, fmt.Errorf("CryptProtectData: %w", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	result := make([]byte, out.cbData)
	copy(result, unsafe.Slice(out.pbData, out.cbData))
	return result, nil
}

func dpUnprotect(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("CryptUnprotectData: empty input")
	}
	in := dataBlob{cbData: uint32(len(data)), pbData: &data[0]}
	var out dataBlob
	r, _, err := procDecryptData.Call(
		uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return nil, fmt.Errorf("CryptUnprotectData: %w", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	result := make([]byte, out.cbData)
	copy(result, unsafe.Slice(out.pbData, out.cbData))
	return result, nil
}
