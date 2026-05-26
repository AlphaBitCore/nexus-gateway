package wiring

import (
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// RevocationResult holds the store and service created by InitRevocation.
type RevocationResult struct {
	Store   *revocation.Store
	Service *revocation.Service
}

// InitRevocation wires the token revocation pipeline (durable DB store +
// MQ fan-out publisher). Both the store and the service are nil when either
// db or mqProducer is absent; the caller should log the gap and surface a
// 503 on /oauth/revoke in that case.
func InitRevocation(db *store.DB, mqProducer mq.Producer, logger *slog.Logger) RevocationResult {
	if db == nil {
		return RevocationResult{}
	}
	if mqProducer == nil {
		logger.Warn("revocation pipeline not wired: MQ producer missing; /oauth/revoke and admin force-logout will return 503")
		return RevocationResult{}
	}
	store := revocation.NewStore(db.Pool)
	service := revocation.NewService(
		store,
		revocation.NewPublisher(mqProducer),
		"authserver",
	)
	return RevocationResult{Store: store, Service: service}
}
