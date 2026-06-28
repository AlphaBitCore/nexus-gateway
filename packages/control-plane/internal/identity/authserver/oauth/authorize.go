package oauth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

// agentDesktopClientID is the reserved client id used by the desktop agent.
// When a request presents this client id the authorize handler enforces the
// device-binding contract: a binding_id that resolves in the BindingStore and
// whose CodeChallenge matches the authorize-request code_challenge.
const agentDesktopClientID = "agent-desktop"

// pendingAuthzHandleTTL is the lifetime of the authctx handle rendered into
// the hosted login page. It must outlast the slowest realistic end-user
// login (credentials + MFA) but stay short enough that abandoned flows do
// not linger. The value deliberately differs from the store's internal
// janitor cadence (which is based on pendingAuthzTTL); entry expiry is
// driven by ExpiresAt, not the janitor tick.
const pendingAuthzHandleTTL = 5 * time.Minute

// clientLoader is the minimum surface AuthorizeHandler needs from a client
// registry. Splitting this out keeps the handler DB-free under unit tests:
// tests inject a small in-memory fake while production wires in
// *store.ClientStore.
type clientLoader interface {
	GetByID(ctx context.Context, id string) (*store.OAuthClient, error)
}

// AuthorizeDeps carries the collaborators AuthorizeHandler needs. Tests can
// supply a fake Clients loader; Bindings and Pending are trivially
// constructible in-memory stores so they stay concrete.
type AuthorizeDeps struct {
	Clients  clientLoader
	Bindings *store.BindingStore
	Pending  *store.PendingAuthzStore
	Logger   *slog.Logger
}

func (d AuthorizeDeps) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// AuthorizeHandler implements RFC 6749 §4.1.1 authorization-code request
// parsing for the control plane's auth server. PKCE (RFC 7636) is enforced
// S256-only; the "plain" method is rejected unconditionally. For the
// agent-desktop client the handler additionally enforces the device-binding
// contract: a binding_id from /oauth/device-binding must resolve and its
// CodeChallenge must match the authorize-request code_challenge. The
// binding is consumed (single-use) on success.
//
// On validation failure the handler renders an RFC 6749 §5.2 error at the
// auth server rather than reflecting it via redirect; per §4.1.2.1 errors
// caused by an invalid redirect_uri MUST NOT be returned to the
// (untrusted) redirect target.
//
// On success the handler mints an opaque authctx, stashes the parsed
// request in PendingAuthzStore, and 302s to {X-Forwarded-Prefix}/login?authctx=<authctx>.
func AuthorizeHandler(d AuthorizeDeps) echo.HandlerFunc {
	return func(c echo.Context) error {
		q := c.QueryParams()

		responseType := q.Get("response_type")
		if responseType != "code" {
			return WriteOAuthError(c, ErrInvalidRequest,
				"unsupported response_type", http.StatusBadRequest)
		}

		clientID := q.Get("client_id")
		if clientID == "" {
			return WriteOAuthError(c, ErrInvalidClient,
				"client_id required", http.StatusBadRequest)
		}
		client, err := d.Clients.GetByID(c.Request().Context(), clientID)
		if err != nil {
			if errors.Is(err, store.ErrClientNotFound) {
				return WriteOAuthError(c, ErrInvalidClient,
					"unknown client_id", http.StatusBadRequest)
			}
			d.logger().Error("authorize: client lookup failed",
				slog.String("client_id", clientID), slog.Any("err", err))
			return WriteOAuthError(c, ErrServerError,
				"client lookup failed", http.StatusInternalServerError)
		}

		redirectURI := q.Get("redirect_uri")
		if redirectURI == "" || !store.RedirectAllowed(*client, redirectURI) {
			// Per RFC 6749 §4.1.2.1, render the error at the auth server
			// rather than 302-redirecting to an untrusted target.
			return WriteOAuthError(c, ErrInvalidRequest,
				"redirect_uri not registered", http.StatusBadRequest)
		}

		codeChallenge := q.Get("code_challenge")
		codeChallengeMethod := q.Get("code_challenge_method")
		// PKCE is mandatory for every client. The token endpoint
		// unconditionally requires a code_verifier (token.go), so a code minted
		// without a challenge could never be exchanged. Requiring the challenge
		// here keeps authorize and token fail-closed and consistent — there is
		// no per-client opt-out.
		if codeChallenge == "" {
			return WriteOAuthError(c, ErrInvalidRequest,
				"code_challenge required", http.StatusBadRequest)
		}
		if codeChallenge != "" {
			if codeChallengeMethod == "" {
				return WriteOAuthError(c, ErrInvalidRequest,
					"code_challenge_method required when code_challenge is present",
					http.StatusBadRequest)
			}
			if codeChallengeMethod != "S256" {
				return WriteOAuthError(c, ErrInvalidRequest,
					"code_challenge_method must be S256", http.StatusBadRequest)
			}
		}

		state := q.Get("state")
		if state == "" {
			return WriteOAuthError(c, ErrInvalidRequest,
				"state required", http.StatusBadRequest)
		}
		nonce := q.Get("nonce")
		scope := q.Get("scope")

		// agent-desktop device-binding handshake — OPTIONAL for first
		// enrollment, REQUIRED when present.
		//
		// Background: /oauth/device-binding is mTLS-gated; only an
		// already-enrolled agent (with a device cert) can pre-flight a
		// binding. A first-enrollment agent has no cert yet — it cannot
		// generate a binding_id, so we cannot demand one here without
		// creating a chicken-and-egg deadlock (cert is issued by the
		// /things/enroll endpoint AFTER this OAuth flow finishes).
		//
		// Security model for the two paths:
		//   - first-enroll (binding_id absent): PKCE S256 + SSO user
		//     login + loopback-only redirect URI is the bootstrap trust.
		//     The auth code is exchanged at /api/agent/sso-enroll for an
		//     enrollment JWT (purpose=enrollment, single-use via JTI
		//     replay guard, 5-min TTL); the JWT is then traded at
		//     /api/internal/things/enroll (Hub) for a device cert.
		//   - re-auth (binding_id present): the agent already has a cert
		//     and pre-flighted through /oauth/device-binding. We MUST
		//     verify the binding here to keep the mTLS-bound replay
		//     protection working — a request that carries binding_id but
		//     fails verification is rejected loudly.
		var deviceID string
		bindingID := q.Get("binding_id")
		if client.ID == agentDesktopClientID && bindingID != "" {
			entry, ok := d.Bindings.Get(bindingID)
			if !ok || entry.CodeChallenge != codeChallenge {
				return WriteOAuthError(c, ErrInvalidRequest,
					"binding_id unknown or mismatched", http.StatusBadRequest)
			}
			deviceID = entry.DeviceID
			// Bindings are single-use: consume on successful pairing.
			d.Bindings.Delete(bindingID)
		}

		authctx := store.RandomOpaqueToken(32)
		d.Pending.Put(authctx, store.PendingAuthzEntry{
			ClientID:      client.ID,
			RedirectURI:   redirectURI,
			Scope:         scope,
			State:         state,
			Nonce:         nonce,
			CodeChallenge: codeChallenge,
			DeviceID:      deviceID,
			ExpiresAt:     time.Now().Add(pendingAuthzHandleTTL),
		})

		prefix := strings.TrimRight(c.Request().Header.Get("X-Forwarded-Prefix"), "/")
		loginURL := (&url.URL{
			Path:     prefix + "/login",
			RawQuery: url.Values{"authctx": []string{authctx}}.Encode(),
		}).String()
		return c.Redirect(http.StatusFound, loginURL)
	}
}
