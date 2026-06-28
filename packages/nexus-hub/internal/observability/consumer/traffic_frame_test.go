package consumer

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

func TestSplitFrame(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"single record, no newline (legacy)", `{"id":"a"}`, 1},
		{"two records", `{"id":"a"}` + "\n" + `{"id":"b"}`, 2},
		{"trailing delimiter", `{"id":"a"}` + "\n" + `{"id":"b"}` + "\n", 2},
		{"blank lines skipped", `{"id":"a"}` + "\n\n" + `{"id":"b"}`, 2},
		{"empty", ``, 0},
		{"only newlines", "\n\n\n", 0},
	}
	for _, c := range cases {
		if got := len(splitFrame([]byte(c.in))); got != c.want {
			t.Errorf("%s: got %d lines, want %d", c.name, got, c.want)
		}
	}
}

func frameCountingMsg() (*mq.Message, *int32, *int32) {
	var acks, naks int32
	m := &mq.Message{
		Ack: func() error { atomic.AddInt32(&acks, 1); return nil },
		Nak: func() error { atomic.AddInt32(&naks, 1); return nil },
	}
	return m, &acks, &naks
}

// A frame of N records acks the underlying message exactly once, and only after
// every record has resolved — never early, never twice.
func TestFrameAck_AllResolveAcksExactlyOnce(t *testing.T) {
	m, acks, naks := frameCountingMsg()
	f := newFrameAck(m, 3)

	f.resolve(false, 0)
	f.resolve(false, 0)
	if got := atomic.LoadInt32(acks); got != 0 {
		t.Fatalf("acked early after 2/3 records: acks=%d", got)
	}
	f.resolve(false, 0) // last record
	if got := atomic.LoadInt32(acks); got != 1 {
		t.Fatalf("want exactly 1 ack after all records, got %d", got)
	}
	f.resolve(false, 0) // stray extra resolution must be a no-op
	if got := atomic.LoadInt32(acks); got != 1 {
		t.Fatalf("frame acked more than once: acks=%d", got)
	}
	if got := atomic.LoadInt32(naks); got != 0 {
		t.Fatalf("unexpected nak: naks=%d", got)
	}
}

// One record asking to retry naks the whole frame once (redeliver all; dedup
// makes the already-committed siblings idempotent).
func TestFrameAck_AnyNakNaksWholeFrameOnce(t *testing.T) {
	m, acks, naks := frameCountingMsg()
	f := newFrameAck(m, 3)

	f.resolve(false, 0)
	f.resolve(true, 0) // this record needs redelivery
	f.resolve(false, 0)
	if got := atomic.LoadInt32(naks); got != 1 {
		t.Fatalf("want exactly 1 nak, got %d", got)
	}
	if got := atomic.LoadInt32(acks); got != 0 {
		t.Fatalf("frame acked despite a nak: acks=%d", got)
	}
}

// NakWithDelay is preferred when present, and the frame uses the MAX requested
// backoff across its records.
func TestFrameAck_NakWithDelayUsesMaxBackoff(t *testing.T) {
	var delays []time.Duration
	m := &mq.Message{
		Ack:          func() error { return nil },
		Nak:          func() error { t.Fatal("bare Nak used despite NakWithDelay present"); return nil },
		NakWithDelay: func(d time.Duration) error { delays = append(delays, d); return nil },
	}
	f := newFrameAck(m, 2)
	f.resolve(true, 5*time.Second)
	f.resolve(true, 9*time.Second)
	if len(delays) != 1 || delays[0] != 9*time.Second {
		t.Fatalf("want one nak-with-delay of 9s (max), got %v", delays)
	}
}

// forceNak settles the message immediately for a partially-processed frame, and
// later resolutions of the already-handed records are no-ops.
func TestFrameAck_ForceNakSettlesImmediatelyAndIsIdempotent(t *testing.T) {
	m, acks, naks := frameCountingMsg()
	f := newFrameAck(m, 5)

	f.forceNak()
	if got := atomic.LoadInt32(naks); got != 1 {
		t.Fatalf("want 1 nak from forceNak, got %d", got)
	}
	f.resolve(false, 0) // a sibling that did commit later — must not flip to ack
	f.resolve(false, 0)
	if got := atomic.LoadInt32(acks); got != 0 {
		t.Fatalf("frame acked after forceNak: acks=%d", got)
	}
	if got := atomic.LoadInt32(naks); got != 1 {
		t.Fatalf("nak count changed after forceNak: naks=%d", got)
	}
}
