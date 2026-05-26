package wiring

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver"
	authserver_store "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/fleet/store/agentstore"
	systemmetastore "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store/systemmetastore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/sso/handler"
)

// AuthServerDeps groups the inputs for InitAuthServer.
type AuthServerDeps struct {
	Cfg               *config.Config
	DB                *store.DB
	HubClient         *hub.Client
	IAMEngine         *iam.Engine
	RevocationService *revocation.Service
	AuditWriter       *audit.Writer
	Logger            *slog.Logger
}

// InitAuthServer mounts the OAuth/OIDC endpoints onto e and registers the
// SSO-based agent enrollment route. Does nothing when db is nil.
//
// Returns a non-nil error only for hard failures (keystore open / key gen).
// The returned closer must be called during shutdown.
func InitAuthServer(ctx context.Context, e *echo.Echo, d AuthServerDeps) (closer func(), err error) {
	closer = func() {}
	if d.DB == nil {
		d.Logger.Warn("auth server not mounted: database unavailable")
		return closer, nil
	}
	cfg := d.Cfg

	keystoreDir, err := filepath.Abs(cfg.AuthServer.KeystoreDir)
	if err != nil {
		return closer, err
	}
	ks, err := token.OpenKeystore(keystoreDir)
	if err != nil {
		return closer, err
	}
	d.Logger.Info("auth keystore ready", "dir", keystoreDir)

	// One-shot idempotent migration of legacy SSO config.
	if created, err := d.DB.MigrateLegacySSOConfigToIdentityProviders(ctx, d.Logger); err != nil {
		d.Logger.Warn("legacy SSO config migration failed (non-fatal)", "error", err)
	} else if created > 0 {
		d.Logger.Info("legacy SSO config migrated to IdentityProvider rows", "rows_created", created)
	}

	if len(ks.All()) == 0 {
		kid, err := ks.Generate()
		if err != nil {
			return closer, err
		}
		d.Logger.Info("auth keystore: generated initial signing key", "kid", kid)
	}

	authCodeStore := authserver_store.NewAuthCodeStore(5 * time.Minute)
	authserver.Mount(e, authserver.Deps{
		DB:          d.DB.Pool,
		Keystore:    ks,
		Signer:      token.NewSigner(ks),
		Logger:      d.Logger,
		Issuer:      cfg.AuthServer.Issuer,
		AgentLookup: agentstore.New(d.DB.InternalPool()),
		Revocation:  d.RevocationService,
		Audit:       d.AuditWriter,
		AuthCodes:   authCodeStore,
	})

	agentGroup := e.Group("/api/agent")
	agentEnrollHandler := &sso.AgentEnrollHandler{
		AuthCodes: authCodeStore,
		Signer:    token.NewSigner(ks),
		Pool:      d.DB.Pool,
		Meta:      systemmetastore.New(d.DB.Pool),
		IAM:       d.IAMEngine,
		Issuer:    cfg.AuthServer.Issuer,
		Logger:    d.Logger,
	}
	agentEnrollHandler.Init()
	agentGroup.POST("/sso-enroll", agentEnrollHandler.SSOEnroll)

	return agentEnrollHandler.Close, nil
}
