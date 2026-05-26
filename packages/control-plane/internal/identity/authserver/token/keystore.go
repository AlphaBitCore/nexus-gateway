// Package token owns RS256 signing keys and issues JWTs.
package token

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Key is a single RSA signing key held by the keystore.
type Key struct {
	KID       string
	Priv      *rsa.PrivateKey
	CreatedAt time.Time
}

// Keystore owns a set of RS256 signing keys persisted as PEM files in dir.
type Keystore struct {
	dir string
	mu  sync.RWMutex
	set []Key
}

// OpenKeystore loads all *.pem files in dir. If none exist the caller must
// Generate() before signing.
func OpenKeystore(dir string) (*Keystore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir keystore: %w", err)
	}
	ks := &Keystore{dir: dir}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pem") {
			continue
		}
		kid := strings.TrimSuffix(e.Name(), ".pem")
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		blk, _ := pem.Decode(data)
		if blk == nil {
			return nil, fmt.Errorf("decode %s", e.Name())
		}
		priv, err := x509.ParsePKCS1PrivateKey(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		info, _ := e.Info()
		ks.set = append(ks.set, Key{KID: kid, Priv: priv, CreatedAt: info.ModTime()})
	}
	sort.Slice(ks.set, func(i, j int) bool { return ks.set[i].CreatedAt.Before(ks.set[j].CreatedAt) })
	return ks, nil
}

// Generate creates a fresh 2048-bit RSA key, persists it, returns its kid.
func (k *Keystore) Generate() (string, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}
	kid := fmt.Sprintf("key-%d", time.Now().UnixNano())
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	if err := os.WriteFile(filepath.Join(k.dir, kid+".pem"), data, 0o600); err != nil {
		return "", err
	}
	k.mu.Lock()
	k.set = append(k.set, Key{KID: kid, Priv: priv, CreatedAt: time.Now()})
	k.mu.Unlock()
	return kid, nil
}

// All returns a copy of the current key set ordered oldest-first.
func (k *Keystore) All() []Key {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := make([]Key, len(k.set))
	copy(out, k.set)
	return out
}

// ActiveKID returns the most-recent key's kid, or "" if the store is empty.
func (k *Keystore) ActiveKID() string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if len(k.set) == 0 {
		return ""
	}
	return k.set[len(k.set)-1].KID
}

// ByKID looks up a key by its kid.
func (k *Keystore) ByKID(kid string) (Key, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	for _, e := range k.set {
		if e.KID == kid {
			return e, true
		}
	}
	return Key{}, false
}
