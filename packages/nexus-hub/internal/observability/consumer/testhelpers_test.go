package consumer

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/siem"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

func newFakeMQMessage(data []byte) *mq.Message {
	return &mq.Message{
		Data: data,
		Ack:  func() error { return nil },
		Nak:  func() error { return nil },
	}
}

// fakeSink is a test double for siem.Sink.
type fakeSink struct {
	name   string
	sendFn func(events []siem.Event) error
}

func (f *fakeSink) Name() string {
	if f.name != "" {
		return f.name
	}
	return "fake-sink"
}

func (f *fakeSink) Send(_ context.Context, events []siem.Event) error {
	if f.sendFn != nil {
		return f.sendFn(events)
	}
	return nil
}
