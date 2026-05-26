package wiring

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine/rules"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine/senders"
)

// AlertsResult holds the alerting subsystem handles.
type AlertsResult struct {
	Store      *alerting.Store
	Raiser     *alerting.Raiser
	Dispatcher *alerting.DispatcherImpl
	SenderReg  SenderRegAdapter
	RulesReg   RulesRegAdapter
}

// InitAlerts constructs the alerting store, sender registry, dispatcher,
// raiser, and rules registry. Built before the scheduler so quota jobs can
// inject the Raiser at construction time.
func InitAlerts(
	pool *pgxpool.Pool,
	logger *slog.Logger,
) AlertsResult {
	alertStore := alerting.NewStore(pool)

	senderReg := senders.NewRegistry()
	senderReg.Register("webhook", senders.NewWebhook(nil))
	senderReg.Register("slack", senders.NewSlack(nil))
	senderReg.Register("email", senders.NewEmail())
	senderReg.Register("pagerduty", senders.NewPagerDuty(nil))
	senderRegAdapter := SenderRegAdapter{R: senderReg}

	dispatcher := alerting.NewDispatcher(alertStore, senderRegAdapter, logger)
	raiser := alerting.NewRaiser(pool, alertStore, dispatcher, logger)
	rulesReg := rules.NewRegistry(rules.BuiltinRules)

	return AlertsResult{
		Store:      alertStore,
		Raiser:     raiser,
		Dispatcher: dispatcher,
		SenderReg:  senderRegAdapter,
		RulesReg:   RulesRegAdapter{R: rulesReg},
	}
}
