package revocation

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// Topic is the MQ subject for auth revocation events. Queue semantics
// (at-least-once, persisted) matter here: every RS replica must see every
// event. Consumer-group fan-out on the RS side gives each service its own
// copy.
const Topic = "nexus.auth.revocation"

// Publisher serialises Events and enqueues them on Topic.
type Publisher struct {
	prod mq.Producer
}

// NewPublisher binds a Publisher to an mq.Producer.
func NewPublisher(p mq.Producer) *Publisher { return &Publisher{prod: p} }

// Publish serialises ev and enqueues it. Returns the serialisation or
// transport error verbatim.
func (p *Publisher) Publish(ctx context.Context, ev Event) error {
	buf, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("revocation: marshal event: %w", err)
	}
	if err := p.prod.Enqueue(ctx, Topic, buf); err != nil {
		return fmt.Errorf("revocation: enqueue: %w", err)
	}
	return nil
}
