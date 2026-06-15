package wiring

import (
	"errors"
	"fmt"
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
//
// Alert-channel config secrets (SMTP password, Slack bot token, PagerDuty
// routing key, sensitive webhook headers) are encrypted at rest under the
// shared CREDENTIAL_ENCRYPTION_KEY. credentialMasterKey is that key, already
// resolved through the SecretCustody loader — so under
// provider "command" it is the UNWRAPPED plaintext, never the env-delivered
// wrapped blob (feeding a blob here would make Hub read it as 64-hex and fail to
// boot). The key is REQUIRED — nexus-hub fails closed at boot when it is unset
// or malformed rather than silently persisting those secrets as cleartext
// (FU-1). This mirrors the [MUST MATCH] contract the key already carries for
// control-plane / ai-gateway provider-credential encryption.
func InitAlerts(
	pool *pgxpool.Pool,
	credentialMasterKey string,
	logger *slog.Logger,
) (AlertsResult, error) {
	secretCipher, err := alerting.ChannelSecretCipherFromKey(credentialMasterKey)
	if err != nil {
		// Set-but-malformed key: a typo must never silently downgrade to plaintext.
		return AlertsResult{}, fmt.Errorf("alert channel secret cipher init: %w", err)
	}
	if secretCipher == nil {
		// Unset key: fail closed. Alert-channel secrets would otherwise be stored
		// in cleartext in the "AlertChannel".config JSONB column.
		return AlertsResult{}, errors.New(
			"CREDENTIAL_ENCRYPTION_KEY is required: nexus-hub encrypts alert-channel " +
				"secrets at rest and refuses to boot without it (generate via `openssl rand -hex 32`)",
		)
	}
	alertStore := alerting.NewStore(pool).WithChannelSecretCipher(secretCipher)

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
	}, nil
}
