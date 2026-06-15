package wiring

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/jwt"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// revocationCatchupInterval is how often InitJWT replays the auth server's
// revocation log via RunCatchup after the initial startup backfill. It is
// shorter than typical token lifetimes so a revocation missed during an MQ gap
// is reconciled well before the affected token would otherwise be honored.
const revocationCatchupInterval = 60 * time.Second

// sanitizeForJetStreamDurable replaces characters NATS JetStream rejects in
// durable-consumer name components (dot, slash, colon, whitespace) with
// underscore. Operator-supplied yaml ids and FQDN-style hostnames can carry
// dots that would otherwise break stream binding.
func sanitizeForJetStreamDurable(s string) string {
	return strings.NewReplacer(".", "_", "/", "_", ":", "_", " ", "_").Replace(s)
}

// InitJWT wires the admin JWT verifier with a bloom-backed revocation checker
// when an MQ consumer is available. Falls back to jwtverifier.AlwaysAllow
// when mqConsumer is nil.
//
// The revocation channel uses JetStream Consume with a per-CP-instance
// durable group so every replica receives every revocation event (the
// fan-out semantics revocation/publisher.go requires). The group name
// derives from the CP's ThingID — operators running multiple CPs in HA
// must give each instance a distinct yaml `id` (or rely on the default
// hostname-based derivation) to avoid sharing a durable and turning the
// fan-out into a work-queue.
//
// The returned goroutine is already started; it exits when ctx is cancelled.
func InitJWT(
	ctx context.Context,
	cfg *config.Config,
	mqConsumer mq.Consumer,
	cpThingID string,
	logger *slog.Logger,
) *jwtverifier.Verifier {
	var adminRevCheck jwtverifier.RevocationChecker = jwtverifier.AlwaysAllow{}
	if mqConsumer != nil {
		introspectURL := cfg.AuthServer.RevocationIntrospectURL
		if introspectURL == "" {
			introspectURL = strings.TrimRight(cfg.AuthServer.Issuer, "/") + "/oauth/introspect"
		}
		replayURL := cfg.AuthServer.RevocationReplayURL
		if replayURL == "" {
			replayURL = strings.TrimRight(cfg.AuthServer.Issuer, "/") + "/api/admin/revocations"
		}
		revChecker := jwtverifier.NewMQRevocationChecker(jwtverifier.MQCheckerConfig{
			IntrospectURL:    introspectURL,
			ReplayURL:        replayURL,
			ReplayAuthHeader: "Bearer " + cfg.Auth.InternalServiceToken,
			Logger:           logger,
		})
		group := "cp-revocation-" + sanitizeForJetStreamDurable(cpThingID)
		go func() {
			err := revChecker.StartConsumer(ctx, func(ctx context.Context, h func(context.Context, []byte) error) error {
				return mqConsumer.Consume(ctx, revocation.Topic, group, func(ctx context.Context, msg *mq.Message) error {
					return h(ctx, msg.Data)
				})
			})
			if err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("revocation consumer exited", slog.Any("err", err))
			}
		}()
		// Backfill revocations missed while this replica was down (the MQ
		// durable's DeliverAll only replays events still retained by JetStream),
		// then poll the auth server's replay endpoint periodically so a gap in
		// MQ delivery is reconciled even before strict mode engages. RunCatchup
		// short-circuits to nil when ReplayURL is unset (dev).
		go func() {
			runCatchup := func() {
				if err := revChecker.RunCatchup(ctx); err != nil && !errors.Is(err, context.Canceled) {
					logger.Warn("revocation catchup failed", slog.Any("err", err))
				}
			}
			runCatchup()
			ticker := time.NewTicker(revocationCatchupInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					runCatchup()
				}
			}
		}()
		adminRevCheck = revChecker
	} else {
		logger.Warn("MQ driver unset -- admin revocation checker will accept all tokens (AlwaysAllow fallback)")
	}

	return jwtverifier.New(jwtverifier.Config{
		Issuer:   cfg.AuthServer.Issuer,
		JWKSURL:  strings.TrimRight(cfg.AuthServer.Issuer, "/") + "/.well-known/jwks.json",
		Audience: token.AdminAudience,
		RevCheck: adminRevCheck,
		Logger:   logger,
	})
}
