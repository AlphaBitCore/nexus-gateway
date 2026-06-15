package alerting

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keycheck"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keyderive"
)

// ChannelSecretCipher encrypts the secret-valued fields of an AlertChannel
// config blob (SMTP password, Slack bot token, PagerDuty routing key,
// sensitive webhook headers) before they are persisted to the
// "AlertChannel".config JSONB column, and decrypts them when a channel is
// loaded for dispatch.
//
// It reuses the repository's credential-encryption mechanism — AES-256-GCM
// keyed by the shared CREDENTIAL_ENCRYPTION_KEY env var (the same 32-byte key
// that encrypts provider API keys at rest, see
// packages/control-plane/internal/platform/crypto). No new scheme and no new
// secret are introduced: the algorithm (AES-256-GCM, 12-byte IV, 16-byte tag)
// and the key source are identical; only the on-wire envelope differs because
// alert secrets live inside a JSONB map rather than dedicated cipher columns.
//
// A nil *ChannelSecretCipher is a valid passthrough — Encrypt/Decrypt return
// the config unchanged — but the hub boot wiring (InitAlerts) treats a nil
// cipher as a fatal error and refuses to start, so a running hub always holds
// a non-nil cipher (FU-1: CREDENTIAL_ENCRYPTION_KEY is required, never
// downgraded to cleartext at rest).
type ChannelSecretCipher struct {
	key []byte // 32 bytes (AES-256)
}

const (
	secretEnvelopePrefix = "enc:v1:"
	secretIVLength       = 12
)

// secretRandReader is the entropy source for IV generation. It is a
// package-level seam so a test can substitute a failing reader and exercise
// the IV-generation error branch; production never reassigns it.
var secretRandReader io.Reader = rand.Reader

// NewChannelSecretCipher builds a cipher from the raw 32-byte master key.
//
// The alert-channel secret class is cryptographically separated from
// the provider-credential class. Rather than use the shared CREDENTIAL_ENCRYPTION_KEY
// master directly as the AES key (which would mean a single key seals two
// unrelated secret classes), the constructor HKDF-derives a class-scoped sub-key
// (keyderive.ClassAlertChannelSecret). The Control Plane provider vault derives a
// DIFFERENT sub-key from the same master, so neither class's AEAD key is the
// master and the two blast radii are distinct.
func NewChannelSecretCipher(key []byte) (*ChannelSecretCipher, error) {
	if len(key) != 32 {
		return nil, errors.New("channel secret cipher: key must be exactly 32 bytes")
	}
	sub, err := keyderive.DeriveKey32(key, keyderive.ClassAlertChannelSecret)
	if err != nil {
		return nil, fmt.Errorf("channel secret cipher: derive class key: %w", err)
	}
	dst := make([]byte, 32)
	copy(dst, sub[:])
	return &ChannelSecretCipher{key: dst}, nil
}

// ChannelSecretCipherFromKey builds a cipher from the CREDENTIAL_ENCRYPTION_KEY
// plaintext (64 hex chars / 32 bytes) the caller already resolved through the
// SecretCustody loader — so under provider "command" this
// receives the UNWRAPPED key, never the env-delivered wrapped blob. It returns
// (nil, nil) when the key is empty and a hard error when it is set-but-malformed
// (so a typo never silently downgrades to plaintext). The empty → (nil, nil)
// case is NOT a license to run without encryption: the boot policy lives one
// layer up in InitAlerts, which rejects a nil cipher and fails the hub closed
// (FU-1).
func ChannelSecretCipherFromKey(key string) (*ChannelSecretCipher, error) {
	keyHex := strings.TrimSpace(key)
	if keyHex == "" {
		return nil, nil
	}
	if len(keyHex) != 64 {
		return nil, errors.New("CREDENTIAL_ENCRYPTION_KEY must be exactly 64 hex characters (32 bytes)")
	}
	keyBytes, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("CREDENTIAL_ENCRYPTION_KEY is not valid hex: %w", err)
	}
	// Refuse a degenerate / known-constant master key (symmetric with
	// the Control Plane + AI Gateway — the same [MUST MATCH] secret).
	if err := keycheck.ValidateMasterKey(keyBytes); err != nil {
		return nil, fmt.Errorf("CREDENTIAL_ENCRYPTION_KEY rejected: %w", err)
	}
	return NewChannelSecretCipher(keyBytes)
}

// seal encrypts plaintext and returns a self-describing envelope string:
//
//	enc:v1:<iv-hex>:<ciphertext+tag-hex>
func (c *ChannelSecretCipher) seal(plaintext string) (string, error) {
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", fmt.Errorf("channel secret: create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("channel secret: create GCM: %w", err)
	}
	iv := make([]byte, secretIVLength)
	if _, err := io.ReadFull(secretRandReader, iv); err != nil {
		return "", fmt.Errorf("channel secret: generate IV: %w", err)
	}
	sealed := aead.Seal(nil, iv, []byte(plaintext), nil)
	return secretEnvelopePrefix + hex.EncodeToString(iv) + ":" + hex.EncodeToString(sealed), nil
}

// open decrypts a value produced by seal. A value WITHOUT the envelope prefix
// is returned unchanged (treated as already-cleartext) so the decrypt path is
// idempotent and tolerant of a never-encrypted field.
func (c *ChannelSecretCipher) open(s string) (string, error) {
	if !strings.HasPrefix(s, secretEnvelopePrefix) {
		return s, nil
	}
	rest := strings.TrimPrefix(s, secretEnvelopePrefix)
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 {
		return "", errors.New("channel secret: malformed envelope")
	}
	iv, err := hex.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("channel secret: decode IV: %w", err)
	}
	if len(iv) != secretIVLength {
		return "", fmt.Errorf("channel secret: invalid IV length: got %d, want %d", len(iv), secretIVLength)
	}
	sealed, err := hex.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("channel secret: decode ciphertext: %w", err)
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", fmt.Errorf("channel secret: create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("channel secret: create GCM: %w", err)
	}
	plaintext, err := aead.Open(nil, iv, sealed, nil)
	if err != nil {
		return "", fmt.Errorf("channel secret: decrypt: %w", err)
	}
	return string(plaintext), nil
}

// encryptConfig returns a deep copy of cfg with every secret-valued field
// (the same key set maskChannelConfig recognises) replaced by its encrypted
// envelope. A nil cipher returns cfg unchanged. Already-encrypted values
// (envelope prefix present) are left as-is so a re-save never double-encrypts.
func (c *ChannelSecretCipher) encryptConfig(cfg map[string]any) (map[string]any, error) {
	return c.transformConfig(cfg, c.sealField)
}

// decryptConfig is the inverse of encryptConfig: every secret-valued field is
// decrypted back to cleartext for the sender. A nil cipher returns cfg
// unchanged.
func (c *ChannelSecretCipher) decryptConfig(cfg map[string]any) (map[string]any, error) {
	return c.transformConfig(cfg, c.openField)
}

func (c *ChannelSecretCipher) sealField(s string) (string, error) {
	if strings.HasPrefix(s, secretEnvelopePrefix) {
		return s, nil // already encrypted
	}
	return c.seal(s)
}

func (c *ChannelSecretCipher) openField(s string) (string, error) {
	return c.open(s)
}

// transformConfig walks the sensitive top-level keys and the sensitive header
// entries, applying fn to each string value. Non-secret keys and non-string
// secret values pass through untouched.
func (c *ChannelSecretCipher) transformConfig(cfg map[string]any, fn func(string) (string, error)) (map[string]any, error) {
	if c == nil || cfg == nil {
		return cfg, nil
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		lk := strings.ToLower(k)
		if sensitiveKeys[lk] {
			if s, ok := v.(string); ok && s != "" {
				transformed, err := fn(s)
				if err != nil {
					return nil, err
				}
				out[k] = transformed
				continue
			}
		}
		if lk == "headers" {
			if sub, ok := v.(map[string]any); ok {
				transformed, err := c.transformHeaders(sub, fn)
				if err != nil {
					return nil, err
				}
				out[k] = transformed
				continue
			}
		}
		out[k] = v
	}
	return out, nil
}

func (c *ChannelSecretCipher) transformHeaders(h map[string]any, fn func(string) (string, error)) (map[string]any, error) {
	out := make(map[string]any, len(h))
	for k, v := range h {
		if headerKeyIsSensitive(k) {
			if s, ok := v.(string); ok && s != "" {
				transformed, err := fn(s)
				if err != nil {
					return nil, err
				}
				out[k] = transformed
				continue
			}
		}
		out[k] = v
	}
	return out, nil
}
