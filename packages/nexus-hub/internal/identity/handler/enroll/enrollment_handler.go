package enroll

import (
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

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/agentca"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/store/enrollstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
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

	// jtiSeen is the in-memory JTI replay guard for enrollment JWTs.
	// Entries auto-expire at the JWT's own `exp` claim and are swept
	// every minute (see jti_cache.go). Hub restarts clear the cache,
	// which is acceptable (in-flight enrollments are invalidated, not
	// replayed) at single-Hub scope.
	jtiSeen *jtiCache
}

// Init wires lazily-initialised internal state. Idempotent. Callers
// who construct an EnrollmentAPI literal must invoke this before
// serving traffic so the JTI cache's sweep goroutine is running.
func (h *EnrollmentAPI) Init() {
	if h.jtiSeen == nil {
		h.jtiSeen = newJTICache()
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
type EnrollRequest struct {
	ThingType string `json:"thingType"`
	ThingID   string `json:"thingId"`
	Version   string `json:"version"`
	CsrPEM    string `json:"csrPem"`
	Hostname  string `json:"hostname"`
	OS        string `json:"os"`
	OSVersion string `json:"osVersion"`
	// DeviceFingerprint, when non-empty, is matched against existing
	// thing rows (type='agent') to dedupe re-enrollments from the same
	// physical host. See enrollWithJWT for the lookup path. Older agent
	// builds that pre-date this field send "" and fall back to the
	// always-mint-new behaviour.
	DeviceFingerprint string `json:"deviceFingerprint"`
	// AttestationCsrPem is the optional Ed25519 CSR the agent ships
	// alongside the mTLS P-256 CSR for traffic attestation. Hub signs it
	// via agentca.SignAttestationCSR (Ed25519-only, no ClientAuth EKU) and
	// returns the cert in AttestationCertPem on the response. Empty when
	// re-enrolling from an older agent build; Hub tolerates absence and the
	// agent runs without attestation until the next re-enroll picks up an
	// updated build.
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
func (h *EnrollmentAPI) verifyEnrollmentJWT(tokenStr string) (userID, email string, retErr error) {
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
	if h.jtiSeen == nil {
		// Defensive: callers that forgot to invoke Init still get a
		// safe fallback. Drops replay protection on this request only;
		// log so the misconfiguration is visible.
		h.logger().Error("jti cache uninitialised; allowing JWT without replay guard")
	} else if !h.jtiSeen.MarkSeen(claims.ID, claims.ExpiresAt.Time) {
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
	if req.CsrPEM == "" {
		return badRequest(c, "csrPem is required")
	}

	authHeader := c.Request().Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		return h.enrollWithJWT(c, strings.TrimPrefix(authHeader, "Bearer "), req)
	}

	enrollToken := c.Request().Header.Get("X-Enrollment-Token")
	if enrollToken == "" {
		return c.JSON(http.StatusUnauthorized, ErrorResponse{
			Error: "Authorization: Bearer or X-Enrollment-Token header required",
			Code:  "UNAUTHORIZED",
		})
	}
	return h.enrollWithToken(c, enrollToken, req)
}

// enrollWithToken is the existing X-Enrollment-Token path (unchanged logic).
func (h *EnrollmentAPI) enrollWithToken(c echo.Context, enrollToken string, req EnrollRequest) error {
	ctx := c.Request().Context()

	token, valid := h.Enrollment.ValidateToken(ctx, enrollToken)
	if !valid {
		return c.JSON(http.StatusUnauthorized, ErrorResponse{
			Error: "invalid, expired, or already used enrollment token",
			Code:  "UNAUTHORIZED",
		})
	}

	thingType := req.ThingType
	if thingType == "" {
		thingType = token.ThingType
	}
	thingID, err := h.resolveThingID(thingType, req.ThingID)
	if err != nil {
		return internalError(c, "thing id generation failed")
	}

	resp, err := h.doEnroll(c, req, thingID, thingType)
	if err != nil {
		return err
	}

	// No DeviceAssignment in this path → trust_level stays at level 1.
	resp["trustLevel"] = h.Mgr.ComputeAndStoreTrustLevel(ctx, thingID, "active", "")

	if err := h.Enrollment.MarkUsed(ctx, token.ID, thingID); err != nil {
		h.logger().Warn("mark enrollment token used failed", "thing_id", thingID, "error", err)
	}

	return c.JSON(http.StatusOK, resp)
}

// enrollWithJWT is the Bearer enrollment JWT path (enterprise-login mode).
func (h *EnrollmentAPI) enrollWithJWT(c echo.Context, tokenStr string, req EnrollRequest) error {
	if h.JWKSCache == nil {
		return c.JSON(http.StatusServiceUnavailable, ErrorResponse{
			Error: "enrollment JWT verification not configured (cpJWKSURL missing)",
			Code:  "JWKS_UNAVAILABLE",
		})
	}

	userID, email, err := h.verifyEnrollmentJWT(tokenStr)
	if err != nil {
		errStr := err.Error()
		switch errStr {
		case "JWKS_UNAVAILABLE":
			return c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: errStr, Code: "JWKS_UNAVAILABLE"})
		case "JWT_REPLAYED":
			return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: errStr, Code: "JWT_REPLAYED"})
		default:
			return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: errStr, Code: "JWT_INVALID"})
		}
	}

	thingType := req.ThingType
	if thingType == "" {
		// The whole platform's convention is `agent` for end-user desktop
		// agents — configreconcile Watch entries, Hub's ThingRollup5mJob,
		// admin_thing_overrides RBAC, IAM ResourceDeviceEnrollment all key
		// on this string. An earlier default of "agent-desktop" silently
		// orphaned newly-enrolled things from every config-push pipeline
		// (the configreconcile SQL `WHERE type='agent'` filtered them out),
		// so things enrolled via SSO never received agent_settings updates.
		// See [[agent-desktop-type-mismatch-bug]].
		thingType = "agent"
	}

	// physical_id dedupe: if the client sent a hardware-stable
	// fingerprint AND we already have an agent thing carrying it as
	// physical_id, reuse that thing_id instead of minting a new one.
	// Without this the fleet accumulates a fresh row every time the
	// .pkg is reinstalled or a second SSO account enrolls from the
	// same Mac. Empty fingerprint (older agent, sandboxed runtime)
	// silently falls through. Lookup hits the indexed `physical_id`
	// column directly — no more jsonb#>> scan.
	var thingID string
	if req.DeviceFingerprint != "" && req.ThingID == "" && thingType == "agent" {
		if existing, lookupErr := h.Mgr.Store().RegistryStore().FindAgentByPhysicalID(c.Request().Context(), req.DeviceFingerprint); lookupErr == nil && existing != "" {
			thingID = existing
			h.logger().Info("sso enroll: reusing existing thing_id by physical_id",
				"thing_id", thingID, "physical_id", req.DeviceFingerprint)
		}
	}
	if thingID == "" {
		var err error
		thingID, err = h.resolveThingID(thingType, req.ThingID)
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

// resolveThingID returns the provided thingID or generates a random one.
func (h *EnrollmentAPI) resolveThingID(thingType, thingID string) (string, error) {
	if thingID != "" {
		return thingID, nil
	}
	b := make([]byte, 8)
	if _, err := io.ReadFull(enrollRandReader, b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%x", thingType, b), nil
}

// doEnroll signs the CSR, registers the thing, and stores credentials.
// It returns the response payload to write back to the client but
// does NOT write it: callers are expected to attach the final
// trust_level (which depends on whether a DeviceAssignment was
// created in this transaction) before serialising.
//
// On any internal failure doEnroll writes an error response and
// returns a non-nil error so callers can short-circuit; Echo's
// middleware sees the headers already sent and skips its default
// rendering.
func (h *EnrollmentAPI) doEnroll(c echo.Context, req EnrollRequest, thingID, thingType string) (map[string]any, error) {
	ctx := c.Request().Context()

	certResult, err := h.CA.SignCSR(req.CsrPEM, fmt.Sprintf("device-%s", thingID))
	if err != nil {
		_ = c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: fmt.Sprintf("CSR signing failed: %s", err.Error()),
			Code:  "BAD_REQUEST",
		})
		return nil, fmt.Errorf("doEnroll: CSR signing failed: %w", err)
	}

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

	if err := h.Mgr.Store().RegistryStore().StoreDeviceTokenHash(ctx, thingID, hashedToken); err != nil {
		_ = internalError(c, "device token storage failed")
		return nil, fmt.Errorf("doEnroll: device token storage: %w", err)
	}

	if err := h.Mgr.Store().RegistryStore().UpdateThingAgent(ctx, store.UpsertThingAgentParams{
		ThingID:       thingID,
		Hostname:      req.Hostname,
		OS:            req.OS,
		OSVersion:     req.OSVersion,
		CertSerial:    certResult.Serial,
		CertExpiresAt: &certResult.ExpiresAt,
	}); err != nil {
		h.logger().Warn("thing_agent upsert failed", "thing_id", thingID, "error", err)
	}

	// physical_id is the dedupe key going forward. RegisterThing has
	// already written it into thing.physical_id via the UpsertThing
	// path (UpsertThingParams.PhysicalID); this branch is a defensive
	// re-stamp for legacy code paths that bypass that struct field
	// (none today, but it's cheap insurance against a missed call site
	// re-introducing the original bug).
	if req.DeviceFingerprint != "" {
		if err := h.Mgr.Store().RegistryStore().SetPhysicalID(ctx, thingID, req.DeviceFingerprint); err != nil {
			h.logger().Warn("set physical_id failed",
				"thing_id", thingID, "physical_id", req.DeviceFingerprint, "error", err)
		}
	}

	resp := map[string]any{
		"id":                   thingID,
		"deviceToken":          plainToken,
		"certPem":              certResult.CertPEM,
		"caCertPem":            certResult.CaCertPEM,
		"certSerial":           certResult.Serial,
		"certExpiresAt":        certResult.ExpiresAt,
		"heartbeatIntervalSec": 30,
		"desired":              regResp.Desired,
		"desiredVer":           regResp.DesiredVer,
	}

	// Attestation cert (optional). The agent ships an Ed25519 CSR
	// alongside the mTLS P-256 CSR; when present, Hub signs it with
	// SignAttestationCSR (Ed25519-only + DigitalSignature KeyUsage,
	// no ClientAuth EKU — enforces the key-separation invariant from
	// the architecture doc) and returns the cert. The public-key bytes
	// are stamped into thing_agent.sysinfo so the compliance-proxy can
	// look them up at verify time via /api/internal/things/:id/
	// attestation-pubkey. Failures here are non-fatal: we log + skip
	// so an Ed25519 issuance error never breaks the mTLS enrollment
	// the agent depends on. The signer ships the cert ONLY when the
	// surrounding mTLS enrollment succeeded, so callers can't observe
	// a half-enrolled state.
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
