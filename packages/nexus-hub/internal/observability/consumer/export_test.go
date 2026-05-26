package consumer

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// InsertTrafficEventsForTest exposes the package-private insert helper to
// black-box tests in the consumer_test package. Mirrors the real batch path:
// callers wrap the call in their own tx and commit on success.
func (w *TrafficEventWriter) InsertTrafficEventsForTest(ctx context.Context, tx pgx.Tx, events []TrafficEventMessage) error {
	items := make([]pendingTrafficMessage, 0, len(events))
	for _, e := range events {
		items = append(items, pendingTrafficMessage{event: e, msg: nil})
	}
	return w.insertTrafficEvents(ctx, tx, items)
}
