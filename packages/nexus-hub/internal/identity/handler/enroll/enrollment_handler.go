package enroll

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/agentca"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/enrollment"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/store/enrollstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillupload"
	nexushttperr "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/httperr"
)

// enrollRandReader is the entropy source for resolveThingID's random ID
// generation. It is a package-level variable solely so tests can substitute a
// failing reader to exercise the entropy-error branch; production code never
// reassigns it. Matches the seam pattern used by packages/nexus-hub/internal/identity/agentca.
var enrollRandReader io.Reader = rand.Reader

// deviceTokenGenFn is the function used to generate a device token during
// enrollment. It is a package-level variable solely so tests can substitute a
// failing function to exercise the token-generation error branch; production
// code never reassigns it.
var deviceTokenGenFn = agentca.GenerateDeviceToken

// timeNow is the clock used to stamp device-token expiry. A package-level seam
// so tests can pin the issue time and assert the computed expiry deterministically;
// production code never reassigns it.
var timeNow = time.Now

// extractAttestationPublicKeyBytes parses the PEM-encoded attestation
// certificate and returns the raw 32-byte Ed25519 public key. The
// enrollment handler stamps this into thing_agent.sysinfo so the
// compliance-proxy can verify signed attestation headers without
// re-parsing the cert chain on every request.
func extractAttestationPublicKeyBytes(certPEM string) ([]byte, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("attestation cert PEM decode failed")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse attestation cert: %w", err)
	}
	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("attestation cert public key is %T, want ed25519.PublicKey", cert.PublicKey)
	}
	return []byte(pub), nil
}

// EnrollmentAPI handles agent enrollment via POST /api/internal/things/enroll.
type EnrollmentAPI struct {
	Mgr        FleetManager
	Enrollment EnrollmentSvc
	CA         CertAuthority
	// JWKSCache verifies enrollment JWTs in enterprise-login mode.
	// When nil, Bearer-token enrollment is rejected with 503.
	JWKSCache JWKSKeyGetter
	// CpIssuer is pinned via jwt.WithIssuer. Required when JWKSCache is
	// non-nil; otherwise a third-party signer that publishes keys on
	// CpJWKSURL could impersonate the Control Plane.
	CpIssuer string
	// Logger captures non-fatal enrollment slips (thing_agent upsert,
	// mark-used). nil falls back to slog.Default.
	Logger *slog.Logger

	// JTIDedup is the optional Redis SETNX one-shot consumer that backs the
	// enrollment-JWT replay guard across Hub restarts and multi-Hub HA.
	// Wired from the shared Redis dedup (the same primitive the
	// spill-upload flow uses). nil → the in-memory L1 guard only (legacy
	// single-Hub-uptime behaviour).
	JTIDedup spillupload.Dedup

	// jtiSeen is the JTI replay guard for enrollment JWTs: an in-process L1
	// (auto-expiring at the JWT's `exp`, swept every minute) in front of the
	// optional Redis L2 (JTIDedup). With the L2 wired, a captured JWT can no
	// longer be replayed across a Hub restart.
	jtiSeen *jtiCache
}

// Init wires lazily-initialised internal state. Idempotent. Callers
// who construct an EnrollmentAPI literal must invoke this before
// serving traffic so the JTI cache's sweep goroutine is running.
func (h *EnrollmentAPI) Init() {
	if h.jtiSeen == nil {
		h.jtiSeen = newJTICache(h.JTIDedup, h.logger())
	}
}

// Close stops background goroutines owned by the handler. Idempotent.
func (h *EnrollmentAPI) Close() {
	if h.jtiSeen != nil {
		h.jtiSeen.Stop()
	}
}

func (h *EnrollmentAPI) logger() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// EnrollRequest is the body for agent enrollment.
//
// There is deliberately NO caller-supplied thing ID: the Hub always assigns
// the thing ID server-side (random mint, or reuse of an existing row matched by
// DeviceFingerprint). A client-chosen ID would let any holder of a valid
// enrollment credential name — and overwrite — an existing Thing, taking over
// another agent or a service identity. Legitimate clients never need to choose
// an ID; re-enrollment from the same host is handled by DeviceFingerprint.
type EnrollRequest struct {
	ThingType string `json:"thingType"`
	Version   string `json:"version"`
	Hostname  string `json:"hostname"`
	OS        string `json:"os"`
	OSVersion string `json:"osVersion"`
	// DeviceFingerprint, when non-empty, is matched against existing
	// thing rows (type='agent') to dedupe re-enrollments from the same
	// physical host. See enrollWithJWT for the lookup path. Older agent
	// builds that pre-date this field send "" and fall back to the
	// always-mint-new behaviour.
	DeviceFingerprint string `json:"deviceFingerprint"`
	// AttestationCsrPem is the optional Ed25519 CSR the agent ships for
	// traffic attestation. Hub signs it via agentca.SignAttestationCSR
	// (Ed25519-only, no ClientAuth EKU) and returns the cert in
	// AttestationCertPem on the response. Empty when re-enrolling from an
	// older agent build; Hub tolerates absence and the agent runs without
	// attestation until the next re-enroll picks up an updated build.
	AttestationCsrPem string `json:"attestationCsrPem,omitempty"`
}

// enrollmentJWTClaims is the claim set used in SSO enrollment JWTs.
type enrollmentJWTClaims struct {
	jwt.RegisteredClaims
	Email   string `json:"email,omitempty"`
	Purpose string `json:"purpose"`
}

const (
	enrollAudience = "nexus-hub-enrollment"
	enrollPurpose  = "enrollment"
)

// verifyEnrollmentJWT verifies a Bearer enrollment JWT and returns (userID, email, error).
// Errors:
//   - "JWT_INVALID"      — bad sig, wrong aud/iss/purpose, expired, missing jti
//   - "JWT_REPLAYED"     — JTI already used
//   - "JWKS_UNAVAILABLE" — cache empty (CP unreachable since Hub started)
func (h *EnrollmentAPI) verifyEnrollmentJWT(ctx context.Context, tokenStr string) (userID, email string, retErr error) {
	var claims enrollmentJWTClaims
	parserOpts := []jwt.ParserOption{
		jwt.WithAudience(enrollAudience),
		jwt.WithExpirationRequired(),
	}
	// Pin iss when configured. CpIssuer should always be set in
	// production; the conditional exists only to keep unit tests that
	// don't care about issuer pinning short.
	if h.CpIssuer != "" {
		parserOpts = append(parserOpts, jwt.WithIssuer(h.CpIssuer))
	}
	_, err := jwt.ParseWithClaims(tokenStr, &claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		kid, _ := t.Header["kid"].(string)
		key, keyErr := h.JWKSCache.Get(kid)
		if keyErr != nil {
			return nil, keyErr
		}
		return key, nil
	}, parserOpts...)

	if err != nil {
		if strings.Contains(err.Error(), "cache is empty") {
			return "", "", fmt.Errorf("JWKS_UNAVAILABLE")
		}
		return "", "", fmt.Errorf("JWT_INVALID: %w", err)
	}

	if claims.Purpose != enrollPurpose {
		return "", "", fmt.Errorf("JWT_INVALID: wrong purpose %q", claims.Purpose)
	}
	if claims.ID == "" {
		return "", "", fmt.Errorf("JWT_INVALID: missing jti")
	}
	if claims.ExpiresAt == nil {
		// Should never happen because WithExpirationRequired enforces
		// presence, but defend the jtiCache contract anyway.
		return "", "", fmt.Errorf("JWT_INVALID: missing exp")
	}

	// Replay guard: MarkSeen is atomic; false means the JTI was already
	// used in a previous request and we must reject.
	//
	// Fail CLOSED when the cache is uninitialised. A nil jtiSeen
	// means Init() was never called — a wiring bug, never a legitimate
	// runtime state. Proceeding without the replay guard would silently
	// accept replayed enrollment JWTs, so we reject the request outright
	// rather than degrade to no protection. The handler surfaces this as a
	// 503 (see enrollWithJWT) so operators see the misconfiguration.
	if h.jtiSeen == nil {
		h.logger().Error("jti replay store unavailable; rejecting enrollment (Init not called)")
		return "", "", fmt.Errorf("JTI_STORE_UNAVAILABLE")
	}
	if !h.jtiSeen.MarkSeen(ctx, claims.ID, claims.ExpiresAt.Time) {
		return "", "", fmt.Errorf("JWT_REPLAYED")
	}

	return claims.Subject, claims.Email, nil
}

// Enroll handles POST /api/internal/things/enroll.
//
// Credential paths (tried in order):
//  1. Authorization: Bearer <enrollment-jwt> — SSO enrollment (enterprise-login mode)
//  2. X-Enrollment-Token: <opaque-token>     — legacy token enrollment
func (h *EnrollmentAPI) Enroll(c echo.Context) error {
	var req EnrollRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}

	authHeader := c.Request().Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		return h.enrollWithJWT(c, strings.TrimPrefix(authHeader, "Bearer "), req)
	}

	enrollToken := c.Request().Header.Get("X-Enrollment-Token")
	if enrollToken == "" {
		return unauthorized(c, "Authorization: Bearer or X-Enrollment-Token header required")
	}
	return h.enrollWithToken(c, enrollToken, req)
}

// enrollWithToken is the X-Enrollment-Token path.
//
// Single-use is enforced consume-FIRST: the token is atomically
// transitioned pending→used before any enrollment work. The prior
// validate-then-(enroll)-then-mark order let two concurrent requests both pass
// the SELECT and both fully enroll before either marked the token used.
// ConsumeToken makes the row write the race arbiter, so exactly one request
// wins; the rest get ErrAlreadyUsed → 401. The token id is linked to the minted
// thing afterward (best-effort; the token is already spent).
func (h *EnrollmentAPI) enrollWithToken(c echo.Context, enrollToken string, req EnrollRequest) error {
	ctx := c.Request().Context()

	token, err := h.Enrollment.ConsumeToken(ctx, enrollToken)
	if err != nil {
		if errors.Is(err, enrollment.ErrAlreadyUsed) {
			return unauthorized(c, "invalid, expired, or already used enrollment token")
		}
		return internalError(c, "enrollment token consume failed")
	}

	// The enrollment token's ThingType is authoritative: a token issued for an
	// `agent` must not be usable to enroll as `ai-gateway` (or any other type)
	// by setting req.ThingType. Only fall back to the request when the token
	// does not pin a type.
	thingType := token.ThingType
	if thingType == "" {
		thingType = req.ThingType
	}
	thingID, err := h.resolveThingID(thingType)
	if err != nil {
		return internalError(c, "thing id generation failed")
	}

	resp, err := h.doEnroll(c, req, thingID, thingType)
	if err != nil {
		return err
	}

	// No DeviceAssignment in this path → trust_level stays at level 1.
	resp["trustLevel"] = h.Mgr.ComputeAndStoreTrustLevel(ctx, thingID, "active", "")

	// Record the binding on the already-consumed token row. Non-fatal: the
	// token is single-use-spent regardless of whether the link succeeds.
	if err := h.Enrollment.LinkThing(ctx, token.ID, thingID); err != nil {
		h.logger().Warn("link enrollment token thing failed", "thing_id", thingID, "error", err)
	}

	return c.JSON(http.StatusOK, resp)
}

// enrollWithJWT is the Bearer enrollment JWT path (enterprise-login mode).
func (h *EnrollmentAPI) enrollWithJWT(c echo.Context, tokenStr string, req EnrollRequest) error {
	if h.JWKSCache == nil {
		return c.JSON(http.StatusServiceUnavailable, nexushttperr.ErrJSON("enrollment JWT verification not configured (cpJWKSURL missing)", "service_unavailable", "JWKS_UNAVAILABLE"))
	}

	userID, email, err := h.verifyEnrollmentJWT(c.Request().Context(), tokenStr)
	if err != nil {
		errStr := err.Error()
		switch errStr {
		case "JWKS_UNAVAILABLE":
			return c.JSON(http.StatusServiceUnavailable, nexushttperr.ErrJSON(errStr, "service_unavailable", "JWKS_UNAVAILABLE"))
		case "JTI_STORE_UNAVAILABLE":
			return c.JSON(http.StatusServiceUnavailable, nexushttperr.ErrJSON("enrollment replay guard unavailable", "service_unavailable", "JTI_STORE_UNAVAILABLE"))
		case "JWT_REPLAYED":
			return c.JSON(http.StatusUnauthorized, nexushttperr.ErrJSON(errStr, "auth_error", "JWT_REPLAYED"))
		default:
			return c.JSON(http.StatusUnauthorized, nexushttperr.ErrJSON(errStr, "auth_error", "JWT_INVALID"))
		}
	}

	// The Bearer enrollment JWT is a DEVICE-enrollment grant — it is
	// authorized by the device verb admin:device-enrollment.enroll (the grant a
	// normal user holds to enroll their own desktop agent) and carries no
	// authorized thingType. It must NOT be usable to mint a privileged
	// service-type Thing (ai-gateway / compliance-proxy / control-plane): such a
	// Thing inherits that service tier's entire desired state on registration
	// (providers / credentials / virtual_keys / routing_rules / response_cache /
	// ...) and would receive every future service-config invalidation push.
	// Service Things enroll ONLY via the operator-issued enrollment-token path,
	// whose token row pins the type (the enrollWithToken invariant above).
	// Mirror that pin here: the authorized type for this path is
	// always `agent`. Reject a caller-supplied non-agent thingType with 403 (so
	// a spoof attempt is explicit and auditable) rather than silently overriding.
	if req.ThingType != "" && req.ThingType != "agent" {
		return c.JSON(http.StatusForbidden, nexushttperr.ErrJSON(
			"enrollment JWT authorizes agent enrollment only; service-type Things require an operator-issued enrollment token",
			"auth_error", "ENROLL_TYPE_FORBIDDEN"))
	}
	// The whole platform's convention is `agent` for end-user desktop agents —
	// configreconcile Watch entries, Hub's ThingRollup5mJob, admin_thing_overrides
	// RBAC, IAM ResourceDeviceEnrollment all key on this string. An earlier
	// default of "agent-desktop" silently orphaned newly-enrolled things from
	// every config-push pipeline (the configreconcile SQL `WHERE type='agent'`
	// filtered them out), so things enrolled via SSO never received agent_settings
	// updates. See [[agent-desktop-type-mismatch-bug]].
	thingType := "agent"

	// physical_id dedupe: if the client sent a hardware-stable
	// fingerprint AND we already have an agent thing carrying it as
	// physical_id, reuse that thing_id instead of minting a new one.
	// Without this the fleet accumulates a fresh row every time the
	// .pkg is reinstalled or a second SSO account enrolls from the
	// same Mac. Empty fingerprint (older agent, sandboxed runtime)
	// silently falls through. Lookup hits the indexed `physical_id`
	// column directly — no more jsonb#>> scan.
	var thingID string
	if req.DeviceFingerprint != "" && thingType == "agent" {
		if existing, lookupErr := h.Mgr.Store().RegistryStore().FindAgentByPhysicalID(c.Request().Context(), req.DeviceFingerprint); lookupErr == nil && existing != "" {
			// DeviceFingerprint is an attacker-controllable request field,
			// so a fingerprint match alone must NOT rebind a device that belongs to
			// another user. Reuse the existing thing only when it is unowned or
			// already owned by THIS SSO user; a match to another user's device is
			// refused (not silently rebound — that would be a cross-user takeover).
			assignment, asgErr := h.Mgr.Store().EnrollStore().GetActiveDeviceAssignment(c.Request().Context(), existing)
			switch {
			case asgErr != nil:
				// Fail closed: don't reuse on an ownership-check error.
				return internalError(c, "device ownership check failed")
			case assignment != nil && assignment.UserID == userID:
				// Same SSO user re-enrolling their own device — safe to dedup
				// onto the existing thing_id (.pkg reinstall / repeat enroll).
				thingID = existing
				h.logger().Info("sso enroll: reusing existing thing_id by physical_id",
					"thing_id", thingID, "physical_id", req.DeviceFingerprint)
			case assignment != nil:
				// Owned by a DIFFERENT user — refuse rebind.
				h.logger().Warn("sso enroll: physical_id matches a device owned by another user; refusing rebind",
					"thing_id", existing, "owner", assignment.UserID, "enrolling_user", userID)
				return c.JSON(http.StatusConflict, nexushttperr.ErrJSON("this device is already enrolled to another user", "conflict", "DEVICE_OWNED_BY_ANOTHER_USER"))
			default:
				// assignment == nil — the existing thing is UNOWNED
				// (e.g. enrolled via X-Enrollment-Token, trust level 1, no
				// DeviceAssignment). A world-readable DeviceFingerprint
				// (SHA-256 over /etc/machine-id + serial + MAC) is an
				// IDENTIFIER, not an AUTHENTICATOR. Reusing this thing_id would
				// (a) overwrite the existing device's token hash → DoS the
				// victim node's WS auth, and (b) rebind + trust-elevate it to
				// the enrolling user → cross-node takeover. So do NOT claim it
				// by fingerprint alone: leave thingID empty so a NEW thing_id
				// is minted below. A deliberate token→SSO transfer must be
				// admin-mediated, not driven by a guessable fingerprint.
				h.logger().Warn("sso enroll: physical_id matches an unowned/token-enrolled thing; minting a new thing_id (fingerprint is not an authenticator)",
					"existing_thing_id", existing, "physical_id", req.DeviceFingerprint, "enrolling_user", userID)
			}
		}
	}
	if thingID == "" {
		var err error
		thingID, err = h.resolveThingID(thingType)
		if err != nil {
			return internalError(c, "thing id generation failed")
		}
	}

	resp, err := h.doEnroll(c, req, thingID, thingType)
	if err != nil {
		return err
	}

	// Create DeviceAssignment so trust_level immediately reaches 2/3.
	// login_method / ip_address get stamped so the admin "User Devices"
	// tab can show login method / IP address instead of dashes — see
	// fleet_queries.go DeviceAssignmentDetail and UserDevicesTab.tsx.
	ctx := c.Request().Context()
	// Snapshot the prior active assignment before the upsert so the
	// device-assignment audit emit can record BeforeState when an SSO
	// re-enroll rebinds the device to a different user. A read failure
	// here is non-fatal — we proceed with the binding and emit audit
	// with nil BeforeState (logged separately).
	priorAssignment, priorErr := h.Mgr.Store().EnrollStore().GetActiveDeviceAssignment(ctx, thingID)
	if priorErr != nil {
		h.logger().Warn("sso enroll: read prior device assignment (audit BeforeState will be omitted)",
			"thing_id", thingID, "error", priorErr)
	}
	if upsertErr := h.Mgr.Store().EnrollStore().UpsertDeviceAssignment(ctx, store.UpsertDeviceAssignmentParams{
		ThingID:     thingID,
		UserID:      userID,
		Source:      store.DeviceAssignmentSourceSSO,
		LoginMethod: string(store.DeviceAssignmentSourceSSO),
		IPAddress:   c.RealIP(),
	}); upsertErr != nil {
		h.logger().Warn("sso enroll: upsert device assignment", "thing_id", thingID, "user_id", userID, "error", upsertErr)
	} else {
		h.logger().Debug("sso enrollment: device assignment created", "thing_id", thingID, "user_id", userID, "email", email)
		// Audit emit happens only on a successful upsert. The audit
		// catalog declares device-assignment.update as the canonical
		// verb for this binding mutation
		// (packages/shared/identity/iam/catalog_data.go:93). Actor is
		// "nexus-hub" because SSO self-enrollment is a daemon-driven
		// flow — the agent obtains a JWT itself; there is no admin
		// principal in the request. ActorLabel carries the email for
		// observability so SIEM searches by email still find the row.
		var beforeState any
		if priorAssignment != nil {
			beforeState = map[string]any{
				"device_id":    thingID,
				"user_id":      priorAssignment.UserID,
				"source":       priorAssignment.Source,
				"login_method": priorAssignment.LoginMethod,
				"ip_address":   priorAssignment.IPAddress,
				"assigned_at":  priorAssignment.AssignedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
			}
		}
		afterState := map[string]any{
			"device_id":    thingID,
			"user_id":      userID,
			"source":       string(store.DeviceAssignmentSourceSSO),
			"login_method": string(store.DeviceAssignmentSourceSSO),
			"ip_address":   c.RealIP(),
			"bound_at":     time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		}
		if auditErr := h.Mgr.Store().EnrollStore().WriteDeviceAssignmentAudit(ctx, enrollstore.DeviceAssignmentAuditEntry{
			ActorID:     "nexus-hub",
			ActorLabel:  email,
			Action:      "device-assignment.update",
			EntityID:    thingID,
			BeforeState: beforeState,
			AfterState:  afterState,
		}); auditErr != nil {
			// Audit failure is non-fatal — the device-binding mutation has
			// already committed. Warn so dashboards alert on the gap.
			h.logger().Warn("sso enroll: write device assignment audit",
				"thing_id", thingID, "user_id", userID, "error", auditErr)
		}
	}

	// Recompute trust_level now that the DeviceAssignment row exists
	// and write the upgraded value into both DB + cache + response.
	resp["trustLevel"] = h.Mgr.ComputeAndStoreTrustLevel(ctx, thingID, "active", "")

	h.logger().Info("sso enrollment complete", "thing_id", thingID, "user_id", userID, "email", email, "trust_level", resp["trustLevel"])
	return c.JSON(http.StatusOK, resp)
}

// Note: the legacy `findThingByFingerprint` jsonb lookup is gone —
// replaced by Store.FindAgentByPhysicalID against the indexed
// `physical_id` column. See migration 20260521000000_thing_physical_id_column.

// resolveThingID generates a fresh server-assigned thing ID. The caller never
// supplies one (see EnrollRequest): the ID is always minted here so a holder of
// a valid enrollment credential cannot name or overwrite an existing Thing.
func (h *EnrollmentAPI) resolveThingID(thingType string) (string, error) {
	b := make([]byte, 8)
	if _, err := io.ReadFull(enrollRandReader, b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%x", thingType, b), nil
}

// doEnroll mints the device token, registers the thing, and stores
// credentials. It returns the response payload to write back to the
// client but does NOT write it: callers are expected to attach the final
// trust_level (which depends on whether a DeviceAssignment was
// created in this transaction) before serialising.
//
// On any internal failure doEnroll writes an error response and
// returns a non-nil error so callers can short-circuit; Echo's
// middleware sees the headers already sent and skips its default
// rendering.
func (h *EnrollmentAPI) doEnroll(c echo.Context, req EnrollRequest, thingID, thingType string) (map[string]any, error) {
	ctx := c.Request().Context()

	plainToken, hashedToken, err := deviceTokenGenFn()
	if err != nil {
		_ = internalError(c, "device token generation failed")
		return nil, fmt.Errorf("doEnroll: device token generation: %w", err)
	}

	regResp, err := h.Mgr.RegisterThing(ctx, manager.RegisterRequest{
		ID:         thingID,
		Type:       thingType,
		Version:    req.Version,
		PhysicalID: req.DeviceFingerprint,
	})
	if err != nil {
		_ = internalError(c, "thing registration failed")
		return nil, fmt.Errorf("doEnroll: thing registration: %w", err)
	}

	deviceTokenExpiresAt := agentca.DeviceTokenExpiry(timeNow())
	if err := h.Mgr.Store().RegistryStore().StoreDeviceTokenHash(ctx, thingID, hashedToken, deviceTokenExpiresAt); err != nil {
		_ = internalError(c, "device token storage failed")
		return nil, fmt.Errorf("doEnroll: device token storage: %w", err)
	}

	if err := h.Mgr.Store().RegistryStore().UpdateThingAgent(ctx, store.UpsertThingAgentParams{
		ThingID:   thingID,
		Hostname:  req.Hostname,
		OS:        req.OS,
		OSVersion: req.OSVersion,
	}); err != nil {
		h.logger().Warn("thing_agent upsert failed", "thing_id", thingID, "error", err)
	}

	resp := map[string]any{
		"id":                   thingID,
		"deviceToken":          plainToken,
		"deviceTokenExpiresAt": deviceTokenExpiresAt.Format(time.RFC3339),
		"heartbeatIntervalSec": 30,
		"desired":              regResp.Desired,
		"desiredVer":           regResp.DesiredVer,
	}

	// Attestation cert (optional). The agent ships an Ed25519 CSR; when
	// present, Hub signs it with SignAttestationCSR (Ed25519-only +
	// DigitalSignature KeyUsage, no ClientAuth EKU — enforces the
	// key-separation invariant from the architecture doc) and returns the
	// cert. The public-key bytes are stamped into thing_agent.sysinfo so
	// the compliance-proxy can look them up at verify time via
	// /api/internal/things/:id/attestation-pubkey. Failures here are
	// non-fatal: we log + skip so an Ed25519 issuance error never breaks
	// the device-token enrollment the agent depends on.
	if req.AttestationCsrPem != "" {
		if attCert, err := h.CA.SignAttestationCSR(req.AttestationCsrPem, thingID); err == nil {
			resp["attestationCertPem"] = attCert.CertPEM
			if pubBytes, perr := extractAttestationPublicKeyBytes(attCert.CertPEM); perr == nil {
				if serr := h.Mgr.Store().RegistryStore().StoreAttestationPubKey(ctx, thingID, pubBytes, attCert.Serial, attCert.ExpiresAt); serr != nil {
					h.logger().Warn("attestation pubkey storage failed",
						"thing_id", thingID, "error", serr)
				}
			} else {
				h.logger().Warn("attestation pubkey extract failed",
					"thing_id", thingID, "error", perr)
			}
		} else {
			h.logger().Warn("attestation CSR signing failed (continuing without attestation)",
				"thing_id", thingID, "error", err)
		}
	}

	return resp, nil
}
