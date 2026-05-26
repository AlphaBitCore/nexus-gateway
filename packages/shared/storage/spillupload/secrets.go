package spillupload

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// SystemMetadataKey is the row in system_metadata that owns the
// rotating signing-secret map.
const SystemMetadataKey = "hub.spill_upload_secret"

// secretsWire is the JSON shape persisted on
// system_metadata['hub.spill_upload_secret']:
//
//	{
//	  "active": "epoch-2",
//	  "secrets": {
//	    "epoch-1": "<base64>",
//	    "epoch-2": "<base64>"
//	  }
//	}
type secretsWire struct {
	Active  string            `json:"active"`
	Secrets map[string]string `json:"secrets"`
}

// MetadataStore is the narrow read/write surface SecretStore needs from
// the platform DB. Hub's `internal/store.Store` satisfies this
// implicitly; tests inject an in-memory double.
type MetadataStore interface {
	GetSystemMetadata(ctx context.Context, key string) ([]byte, error)
	SetSystemMetadata(ctx context.Context, key string, value any, updatedBy string) error
}

// SecretStore loads the rotating signing-secret map from
// system_metadata, auto-generates an `epoch-1` entry on first boot when
// the row is absent, and serves Active() / Lookup() lock-free over the
// loaded snapshot. Rotation today requires a Hub restart; live rotation
// is left for a follow-up if it becomes operationally necessary.
type SecretStore struct {
	mu      sync.RWMutex
	active  string
	secrets map[string][]byte
}

// LoadOrInit reads system_metadata[SystemMetadataKey], decodes the
// rotation map, and returns a primed SecretStore. When the row is
// missing or its `secrets` map is empty it auto-generates `epoch-1`,
// writes it back, and returns the primed store. Bootstrap happens
// once per fresh deployment.
func LoadOrInit(ctx context.Context, db MetadataStore) (*SecretStore, error) {
	if db == nil {
		return nil, errors.New("spillupload: nil metadata store")
	}
	raw, err := db.GetSystemMetadata(ctx, SystemMetadataKey)
	if err != nil {
		return nil, fmt.Errorf("spillupload: read %s: %w", SystemMetadataKey, err)
	}
	wire := secretsWire{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &wire); err != nil {
			return nil, fmt.Errorf("spillupload: decode %s: %w", SystemMetadataKey, err)
		}
	}
	if len(wire.Secrets) == 0 {
		// First-boot bootstrap: mint epoch-1 and persist.
		secret, err := GenerateSecret()
		if err != nil {
			return nil, err
		}
		wire = secretsWire{
			Active: "epoch-1",
			Secrets: map[string]string{
				"epoch-1": base64.StdEncoding.EncodeToString(secret),
			},
		}
		if err := db.SetSystemMetadata(ctx, SystemMetadataKey, wire, "hub-bootstrap"); err != nil {
			return nil, fmt.Errorf("spillupload: persist epoch-1: %w", err)
		}
	}
	if wire.Active == "" {
		// Self-heal: pick any secret as active so verify still works
		// even if a hand-edited row landed without `active`.
		for k := range wire.Secrets {
			wire.Active = k
			break
		}
	}
	decoded, err := decodeSecrets(wire.Secrets)
	if err != nil {
		return nil, err
	}
	if _, ok := decoded[wire.Active]; !ok {
		return nil, fmt.Errorf("spillupload: active kid %q not in secrets map", wire.Active)
	}
	return &SecretStore{active: wire.Active, secrets: decoded}, nil
}

// NewInMemorySecretStore is a test helper that primes a SecretStore
// with the given map. Production callers go through LoadOrInit.
func NewInMemorySecretStore(active string, secrets map[string][]byte) *SecretStore {
	dup := make(map[string][]byte, len(secrets))
	for k, v := range secrets {
		c := make([]byte, len(v))
		copy(c, v)
		dup[k] = c
	}
	return &SecretStore{active: active, secrets: dup}
}

// Active satisfies SecretSource for the mint path.
func (s *SecretStore) Active() (string, []byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.active == "" {
		return "", nil, errors.New("spillupload: no active secret")
	}
	secret, ok := s.secrets[s.active]
	if !ok {
		return "", nil, fmt.Errorf("spillupload: active kid %q missing in map", s.active)
	}
	return s.active, secret, nil
}

// Lookup satisfies SecretSource for the verify path. Unknown kids
// return ErrUnknownKID so the caller can map to HTTP 401.
func (s *SecretStore) Lookup(kid string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	secret, ok := s.secrets[kid]
	if !ok {
		return nil, ErrUnknownKID
	}
	return secret, nil
}

// decodeSecrets b64-decodes every entry in the wire map. Returns an
// error if any entry fails to decode so an operator who fat-fingered
// a hand edit gets a loud failure at boot rather than at first verify.
func decodeSecrets(in map[string]string) (map[string][]byte, error) {
	out := make(map[string][]byte, len(in))
	for kid, encoded := range in {
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("spillupload: decode secret %q: %w", kid, err)
		}
		if len(raw) < 16 {
			return nil, fmt.Errorf("spillupload: secret %q too short (%d bytes; want >= 16)", kid, len(raw))
		}
		out[kid] = raw
	}
	return out, nil
}

// Dedup is the narrow Redis surface SecretStore-protected blob uploads
// use for one-shot consumption. SetNX returns (true, nil) when the
// caller wins the slot; (false, nil) when a previous PUT already
// consumed the token. Errors propagate so a Redis outage surfaces as
// 503 rather than letting a replay sneak through.
type Dedup interface {
	SetNX(ctx context.Context, key string, ttl time.Duration) (acquired bool, err error)
}

// DedupKey returns the Redis key used to mark a token as consumed.
// Hashing the full token (not just kid+eid+dir) keeps the key size
// bounded and avoids leaking the eventId / direction into Redis logs.
// The token already encodes those fields and the hash is deterministic,
// so dedup remains correct.
func DedupKey(token string) string {
	return "spill_token_used:" + base64.RawURLEncoding.EncodeToString(sha256OfString(token))
}
