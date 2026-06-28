package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"time"

	"github.com/jackc/pgx/v5"
)

// UpsertThingAgentParams holds parameters for inserting/updating a thing_agent row.
type UpsertThingAgentParams struct {
	ThingID       string
	Hostname      string
	OS            string
	OSVersion     string
	CertSerial    string
	CertExpiresAt *time.Time
}

// UpdateThingAgent inserts or updates the thing_agent extension row.
// Hostname/os/os_version live ONLY on thing.* (written by Heartbeat from
// staticInfo); this
// helper only owns the agent-cert columns now. The params struct still
// carries Hostname/OS/OSVersion for backward-compat with callers, but
// the helper writes them into thing.* directly so the move is atomic
// with respect to a single enrollment call site.
func (s *Store) UpdateThingAgent(ctx context.Context, p UpsertThingAgentParams) error {
	// Mirror Hostname/OS/OSVersion onto thing.* so identity is consistent
	// from the moment enrollment finishes (Heartbeat would normally do
	// this on the FIRST static_info push, but the device may not push
	// for several seconds; CP queries reading thing.hostname would see
	// NULL until then). Best-effort — failures don't abort enrollment.
	var hostname, osVal, osVersion any
	if p.Hostname != "" {
		hostname = p.Hostname
	}
	if p.OS != "" {
		osVal = p.OS
	}
	if p.OSVersion != "" {
		osVersion = p.OSVersion
	}
	if hostname != nil || osVal != nil || osVersion != nil {
		if _, err := s.db.Exec(ctx, `
			UPDATE thing
			SET hostname   = COALESCE($2, hostname),
			    os         = COALESCE($3, os),
			    os_version = COALESCE($4, os_version),
			    updated_at = NOW()
			WHERE id = $1
		`, p.ThingID, hostname, osVal, osVersion); err != nil {
			return fmt.Errorf("mirror thing identity: %w", err)
		}
	}

	_, err := s.db.Exec(ctx, `
		INSERT INTO thing_agent (thing_id, cert_serial, cert_expires_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (thing_id) DO UPDATE SET
			cert_serial     = EXCLUDED.cert_serial,
			cert_expires_at = EXCLUDED.cert_expires_at
	`, p.ThingID, p.CertSerial, p.CertExpiresAt)
	if err != nil {
		return fmt.Errorf("upsert thing_agent: %w", err)
	}
	return nil
}

// StoreAttestationPubKey persists the Ed25519 attestation public key
// + cert metadata into thing_agent.sysinfo for the given agent. Used
// by the enrollment handler right after agentca.SignAttestationCSR
// returns the signed cert; the compliance-proxy fetches the public key
// later via /api/internal/things/:id/attestation-pubkey to verify the
// X-Nexus-Attestation header on outbound traffic.
//
// The data lives inside the existing sysinfo JSONB column (jsonb_set merge).
// Schema:
//
//	sysinfo.attestation = {
//	    "publicKey":      "<base64 stdlib NoPadding>",
//	    "certSerial":     "<hex>",
//	    "certExpiresAt":  "<RFC3339>",
//	}
//
// Existing keys under sysinfo are preserved (jsonb_set merge).
func (s *Store) StoreAttestationPubKey(ctx context.Context, thingID string, publicKey []byte, certSerial string, certExpiresAt time.Time) error {
	payload := map[string]any{
		"publicKey":     base64.StdEncoding.EncodeToString(publicKey),
		"certSerial":    certSerial,
		"certExpiresAt": certExpiresAt.UTC().Format(time.RFC3339),
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal attestation payload: %w", err)
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE thing_agent
		SET sysinfo = jsonb_set(COALESCE(sysinfo, '{}'::jsonb), '{attestation}', $2::jsonb, true)
		WHERE thing_id = $1
	`, thingID, string(payloadJSON))
	if err != nil {
		return fmt.Errorf("store attestation pubkey: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetAttestationPubKeyWithExpiry returns the Ed25519 attestation public key plus
// the NotAfter of the attestation certificate that minted it. CP
// uses certExpiresAt to stop trusting a key once its 90-day cert has lapsed —
// without it, a compromised/exfiltrated key would bypass compliance inspection
// forever. A row with a publicKey but no certExpiresAt (legacy stamp) returns a
// zero time, which CP treats as non-expiring (fail-open). ErrNotFound semantics
// match GetAttestationPubKey (empty/absent publicKey → miss → MITM fallback).
func (s *Store) GetAttestationPubKeyWithExpiry(ctx context.Context, thingID string) (pub []byte, certExpiresAt time.Time, err error) {
	var encoded, expiresStr string
	// Revocation: a revoked (unenrolled) device's attestation key MUST
	// stop being served so the Compliance Proxy reverts to MITM inspection.
	// Joining thing and excluding status='revoked' makes this the single
	// trust-decision chokepoint — UnenrollDevice (which sets status='revoked')
	// and any other revocation path immediately stop the key from being handed to
	// CP. A revoked device yields no row → ErrNotFound → 404 → unknown_agent →
	// MITM, bounded by the CP key cache's ≤60s positive TTL.
	err = s.db.QueryRow(ctx, `
		SELECT COALESCE(ta.sysinfo->'attestation'->>'publicKey', ''),
		       COALESCE(ta.sysinfo->'attestation'->>'certExpiresAt', '')
		FROM thing_agent ta
		JOIN thing t ON t.id = ta.thing_id
		WHERE ta.thing_id = $1 AND t.status != 'revoked'
	`, thingID).Scan(&encoded, &expiresStr)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, time.Time{}, ErrNotFound
		}
		return nil, time.Time{}, fmt.Errorf("load attestation pubkey: %w", err)
	}
	if encoded == "" {
		return nil, time.Time{}, ErrNotFound
	}
	raw, derr := base64.StdEncoding.DecodeString(encoded)
	if derr != nil {
		return nil, time.Time{}, fmt.Errorf("decode attestation pubkey: %w", derr)
	}
	if expiresStr != "" {
		if t, perr := time.Parse(time.RFC3339, expiresStr); perr == nil {
			certExpiresAt = t
		}
	}
	return raw, certExpiresAt, nil
}

// StoreDeviceTokenHash stores the SHA-256 hash of a device token in
// thing.metadata and stamps its expiry on the first-class
// device_token_expires_at column. Called both at enrollment (initial issue) and
// on rotation (POST /api/internal/things/renew-token) — in both cases the
// previous hash is overwritten, so the old plaintext token is immediately
// invalidated (a stolen token's lifetime is bounded by the next rotation).
// expiresAt is the absolute expiry computed by agentca.DeviceTokenExpiry.
func (s *Store) StoreDeviceTokenHash(ctx context.Context, thingID, tokenHash string, expiresAt time.Time) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE thing
		SET metadata = jsonb_set(COALESCE(metadata, '{}'), '{deviceTokenHash}', to_jsonb($2::text)),
		    device_token_expires_at = $3,
		    updated_at = NOW()
		WHERE id = $1
	`, thingID, tokenHash, expiresAt)
	if err != nil {
		return fmt.Errorf("store device token hash: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ValidateDeviceToken checks a hashed device token against the stored hash.
// Returns the Thing if valid, not revoked, and not expired. The
// `device_token_expires_at > NOW()` predicate makes expiry enforcement
// fail-closed: a row whose expiry is NULL (never issued a token) or in the past
// (lapsed) is dropped by the WHERE clause and the query returns ErrNoRows, which
// the caller surfaces as an auth rejection — there is no separate expiry branch
// in Go that a future edit could accidentally skip.
func (s *Store) ValidateDeviceToken(ctx context.Context, thingID, tokenHash string) (*Thing, error) {
	t := &Thing{}
	var desiredRaw, reportedRaw, metadataRaw, outcomesRaw []byte
	err := s.db.QueryRow(ctx, `
		SELECT id, type, COALESCE(name,''), COALESCE(version,''), COALESCE(address,''),
		       COALESCE(enrolled_by,''), auth_type, conn_protocol,
		       status, desired, reported, desired_ver, reported_ver, metadata, last_seen_at, enrolled_at,
		       reported_outcomes, process_started_at
		FROM thing
		WHERE id = $1
		  AND metadata->>'deviceTokenHash' = $2
		  AND status != 'revoked'
		  AND device_token_expires_at > NOW()
	`, thingID, tokenHash).Scan(
		&t.ID, &t.Type, &t.Name, &t.Version, &t.Address,
		&t.EnrolledBy, &t.AuthType, &t.ConnProtocol,
		&t.Status, &desiredRaw, &reportedRaw, &t.DesiredVer, &t.ReportedVer,
		&metadataRaw, &t.LastSeenAt, &t.EnrolledAt,
		&outcomesRaw, &t.ProcessStartedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("validate device token: %w", err)
	}
	if err := decodeJSONB(desiredRaw, &t.Desired, "desired"); err != nil {
		return nil, err
	}
	if err := decodeJSONB(reportedRaw, &t.Reported, "reported"); err != nil {
		return nil, err
	}
	if err := decodeJSONB(metadataRaw, &t.Metadata, "metadata"); err != nil {
		return nil, err
	}
	if err := decodeJSONB(outcomesRaw, &t.ReportedOutcomes, "reported_outcomes"); err != nil {
		return nil, err
	}
	return t, nil
}
