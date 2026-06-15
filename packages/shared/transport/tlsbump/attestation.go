package tlsbump

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// AttestationHeaderName is the canonical wire name of the attestation
// header. The header rides on the CONNECT line the agent sends upstream;
// the Compliance-Proxy peeks the value and decides whether to transparently
// tunnel (valid sig) or fall through to its usual MITM path (invalid /
// missing / replayed — fail-open per architecture § 4).
const AttestationHeaderName = "X-Nexus-Attestation"

// AttestationHeaderVersion is the wire version. Future
// revisions add fields without changing the v1 verification semantics —
// the canonical signing pre-image is append-only, so a v2 reader that
// sees a v1 header still verifies cleanly.
const AttestationHeaderVersion = "v1"

// EmptyBodySHA256Hex is the SHA-256 of an empty byte string in lowercase
// hex. The agent emits the attestation header at CONNECT time, BEFORE
// the application body is on the wire, so v1 commits to "no body
// inspected yet" by writing sha256("") in the hash field. CP default
// mode does NOT verify the hash field (architecture § 3.5); strict_mode
// is deferred to v2 where attestation would attach to the inner HTTP
// request rather than the CONNECT line. Hard-coding the constant keeps
// both signer and verifier on the same wire bytes without re-hashing
// per request.
const EmptyBodySHA256Hex = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// AttestationFields is the parsed payload of the X-Nexus-Attestation
// header. Construct via ParseAttestationHeader (verifier side) or
// AttestationFields literal followed by SignatureInput (signer side).
type AttestationFields struct {
	Version   string // "v1"
	TS        int64  // Unix seconds anchor of the replay window
	Nonce     string // 32 lowercase hex chars (16 random bytes)
	Hash      string // "sha256:<64-hex>" — body commitment
	AgentID   string // UUID
	Signature string // base64url(64-byte Ed25519 signature), no padding
}

// SignatureInput returns the canonical pre-image the Ed25519 signature
// covers. Per architecture § 3.4 the form is a newline-separated list
// with a trailing newline included:
//
//	v1\n
//	ts=<ts>\n
//	nonce=<nonce>\n
//	hash=<hash>\n
//	agent_id=<agent_id>\n
//
// Future versions add fields below agent_id without altering v1
// verification semantics.
func (f AttestationFields) SignatureInput() []byte {
	var sb strings.Builder
	sb.Grow(160)
	sb.WriteString(f.Version)
	sb.WriteByte('\n')
	sb.WriteString("ts=")
	sb.WriteString(strconv.FormatInt(f.TS, 10))
	sb.WriteByte('\n')
	sb.WriteString("nonce=")
	sb.WriteString(f.Nonce)
	sb.WriteByte('\n')
	sb.WriteString("hash=")
	sb.WriteString(f.Hash)
	sb.WriteByte('\n')
	sb.WriteString("agent_id=")
	sb.WriteString(f.AgentID)
	sb.WriteByte('\n')
	return []byte(sb.String())
}

// FormatHeader renders the wire-format header value for the agent side.
// Caller must populate every field including Signature.
func (f AttestationFields) FormatHeader() string {
	return AttestationHeaderVersion +
		";ts=" + strconv.FormatInt(f.TS, 10) +
		";nonce=" + f.Nonce +
		";hash=" + f.Hash +
		";agent_id=" + f.AgentID +
		";sig=" + f.Signature
}

// ParseAttestationHeader splits the wire value into its fields. It
// validates shape only — caller is responsible for ts-window check,
// signature verify, and replay-window LRU lookup. Returns nil + error
// on any malformed input so the caller can map the failure to its
// MITM-fallback outcome label.
func ParseAttestationHeader(raw string) (*AttestationFields, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("attestation: header is empty")
	}
	parts := strings.Split(raw, ";")
	if len(parts) < 6 {
		return nil, fmt.Errorf("attestation: expected ≥6 fields, got %d", len(parts))
	}
	if parts[0] != AttestationHeaderVersion {
		return nil, fmt.Errorf("attestation: unsupported version %q", parts[0])
	}

	out := &AttestationFields{Version: AttestationHeaderVersion}
	for _, p := range parts[1:] {
		eq := strings.IndexByte(p, '=')
		if eq < 0 {
			return nil, fmt.Errorf("attestation: field missing '=' in %q", p)
		}
		k, v := p[:eq], p[eq+1:]
		switch k {
		case "ts":
			ts, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("attestation: bad ts %q: %w", v, err)
			}
			out.TS = ts
		case "nonce":
			if len(v) != 32 {
				return nil, fmt.Errorf("attestation: nonce length = %d; want 32 hex chars", len(v))
			}
			if _, err := hex.DecodeString(v); err != nil {
				return nil, fmt.Errorf("attestation: nonce not hex: %w", err)
			}
			out.Nonce = v
		case "hash":
			if !strings.HasPrefix(v, "sha256:") || len(v) != len("sha256:")+64 {
				return nil, fmt.Errorf("attestation: hash must be sha256:<64-hex>, got %q", v)
			}
			if _, err := hex.DecodeString(v[len("sha256:"):]); err != nil {
				return nil, fmt.Errorf("attestation: hash hex invalid: %w", err)
			}
			out.Hash = v
		case "agent_id":
			if v == "" {
				return nil, errors.New("attestation: agent_id empty")
			}
			out.AgentID = v
		case "sig":
			if _, err := base64.RawURLEncoding.DecodeString(v); err != nil {
				return nil, fmt.Errorf("attestation: sig not base64url: %w", err)
			}
			out.Signature = v
		default:
			// Unknown field: ignore for forward-compat. A future v2 may
			// add a field that v1 readers must transparently tolerate.
		}
	}

	if out.TS == 0 || out.Nonce == "" || out.Hash == "" || out.AgentID == "" || out.Signature == "" {
		return nil, errors.New("attestation: required field(s) missing")
	}
	return out, nil
}

// HashEmptyBody returns the canonical hash-field value for a v1 CONNECT
// attestation where no inner body has been emitted yet. Equivalent to
// `"sha256:" + hex(sha256(""))`. Exposed as a constant-time helper so
// both signer and verifier read the same source.
//
// Attestation v1 scope (intentional, not a gap): the Ed25519 signature
// covers (version, ts, nonce, sha256(""), agent_id) — see
// SignatureInput. The empty-body hash is deliberate: this header
// authorizes the AGENT'S IDENTITY at CONNECT time, not a specific
// request body. The body is not yet on the wire when the agent emits the
// header, so committing to sha256("") is the only honest value at v1.
// Replay protection is the verifier's per-(ts,nonce) LRU; the staleness
// bound is the ±5-minute ts window. Body-bound attestation (hashing the
// real inner request) is the deferred v2 strict_mode where attestation
// attaches to the inner HTTP request rather than the CONNECT line.
func HashEmptyBody() string {
	return "sha256:" + EmptyBodySHA256Hex
}

// HashBody returns the v1 hash-field value for a non-empty body. Used by
// future strict_mode (v2) — v1 CONNECT-time attestation calls
// HashEmptyBody instead. Kept here so the wire-format helper lives in
// one package.
func HashBody(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}
