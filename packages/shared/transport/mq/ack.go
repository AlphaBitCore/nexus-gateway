package mq

import "errors"

// ErrDeferAck signals to a Consumer that the handler has taken responsibility
// for calling Ack() or Nak() on the message itself, at some later point
// (typically after a batched DB flush). Returning this sentinel from a
// MessageHandler suppresses both the automatic Ack-on-nil and Nak-on-error
// behaviors.
//
// Example:
//
//	handler := func(_ context.Context, msg *mq.Message) error {
//	    if err := batch.Add(msg); err != nil {
//	        return err // auto-nak
//	    }
//	    return mq.ErrDeferAck // batch will call msg.Ack() after flush
//	}
//
// Consumers that do not recognise ErrDeferAck treat it as any other error
// (Nak). natsmq recognises it.
var ErrDeferAck = errors.New("mq: ack deferred to handler")

// IsDeferAck reports whether the error is or wraps ErrDeferAck.
func IsDeferAck(err error) bool { return errors.Is(err, ErrDeferAck) }
