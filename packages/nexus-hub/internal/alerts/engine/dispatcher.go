package alerting

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// Sender delivers a single alert to a single channel. A concrete Sender lives
// in the senders subpackage (webhook, slack, email, pagerduty) and returns
// the remote HTTP status (0 when no request was made) plus an error if the
// attempt failed.
//
// Declared here so the dispatcher can avoid importing the senders package
// directly — senders already imports alerting for Alert and Channel types,
// and a reverse import would create a cycle.
type Sender interface {
	Send(ctx context.Context, ch Channel, a Alert) (statusCode int, err error)
}

// SenderRegistry resolves a channel type string (webhook, slack, …) to its
// Sender implementation. senders.Registry in the subpackage satisfies this
// interface.
type SenderRegistry interface {
	Get(channelType string) (Sender, error)
}

// DispatcherImpl is the concrete implementation of the Dispatcher interface
// declared in raiser.go. It fans a fired Alert out to every enabled channel
// whose severity + sourceType filters match, writing one AlertDispatch row
// per attempt (success or failure) for operator visibility.
type DispatcherImpl struct {
	store   *Store
	senders SenderRegistry
	logger  *slog.Logger
}

// NewDispatcher constructs a DispatcherImpl. reg is typically a
// *senders.Registry from the sibling subpackage.
func NewDispatcher(store *Store, reg SenderRegistry, logger *slog.Logger) *DispatcherImpl {
	if logger == nil {
		logger = slog.Default()
	}
	return &DispatcherImpl{store: store, senders: reg, logger: logger}
}

// Dispatch delivers a fired Alert to every enabled channel that matches on
// severity + sourceType. Each channel attempt is persisted as an
// AlertDispatch row; a missing sender registration also produces a failure
// row so the UI can surface the misconfiguration.
func (d *DispatcherImpl) Dispatch(ctx context.Context, a Alert) {
	channels, err := d.store.ListEnabledChannels(ctx)
	if err != nil {
		d.logger.Error("dispatcher: list channels", "err", err)
		return
	}
	for _, ch := range channels {
		if !matchesSeverity(ch, a.Severity) || !matchesSourceType(ch, a.SourceType) {
			continue
		}
		s, err := d.senders.Get(ch.Type)
		if err != nil {
			d.writeDispatch(ctx, a.ID, ch, false, nil, err.Error())
			continue
		}
		sc, sendErr := s.Send(ctx, ch, a)
		var scPtr *int
		if sc != 0 {
			scPtr = &sc
		}
		if sendErr != nil {
			d.writeDispatch(ctx, a.ID, ch, false, scPtr, sendErr.Error())
			continue
		}
		d.writeDispatch(ctx, a.ID, ch, true, scPtr, "")
	}
}

// matchesSeverity returns true if the channel's severity filter accepts sev.
// An empty severities list is treated as "match all".
//
// Both sides are typed Severity values now — the admin handler decodes the
// inbound payload via Severity.UnmarshalJSON (rejecting unknowns at the
// 400 boundary), and the store-row scanner funnels DB rows through
// ParseLoose. EqualFold still defends against any rogue mixed-case row
// that slipped past pre-typed-enum writes; once such rows are confirmed
// absent in prod the comparison can collapse to direct equality.
func matchesSeverity(ch Channel, sev Severity) bool {
	if len(ch.Severities) == 0 {
		return true
	}
	target := sev.String()
	for _, s := range ch.Severities {
		if strings.EqualFold(s.String(), target) {
			return true
		}
	}
	return false
}

// matchesSourceType returns true if the channel's sourceType filter accepts
// st. An empty sourceTypes list is treated as "match all". Case-insensitive
// for the same reason as matchesSeverity.
func matchesSourceType(ch Channel, st string) bool {
	if len(ch.SourceTypes) == 0 {
		return true
	}
	for _, s := range ch.SourceTypes {
		if strings.EqualFold(s, st) {
			return true
		}
	}
	return false
}

// writeDispatch persists a single delivery attempt. Failures to write are
// logged but not surfaced: the Alert itself is already persisted, and we
// prefer best-effort dispatch bookkeeping over masking the caller.
func (d *DispatcherImpl) writeDispatch(ctx context.Context, alertID string, ch Channel, ok bool, sc *int, errMsg string) {
	var errPtr *string
	if errMsg != "" {
		errPtr = &errMsg
	}
	_, err := d.store.InsertDispatch(ctx, Dispatch{
		AlertID:     alertID,
		ChannelID:   ch.ID,
		ChannelName: ch.Name,
		Success:     ok,
		StatusCode:  sc,
		ErrorMsg:    errPtr,
		AttemptedAt: time.Now().UTC(),
	})
	if err != nil {
		d.logger.Error("dispatcher: write dispatch row",
			"alertId", alertID,
			"channelId", ch.ID,
			"err", err,
		)
	}
}
