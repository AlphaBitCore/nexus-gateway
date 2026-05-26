package mq

// ComplianceTestSuite runs the standard MQ interface tests against a matched
// Producer/Consumer pair. Each driver's test file calls this to verify it
// satisfies the interface contract.
//
// Usage (in a driver's _test.go file):
//
//	func TestCompliance(t *testing.T) {
//	    producer, consumer := buildYourPair(t)
//	    mq.ComplianceTestSuite(t, producer, consumer)
//	}
//
// This file lives in package mq (not mq_test) so it can be imported by
// sub-packages without an import cycle.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ComplianceTestSuite verifies that a Producer/Consumer pair satisfies
// the mq interface contract.
func ComplianceTestSuite(t *testing.T, producer Producer, consumer Consumer) {
	t.Helper()

	t.Run("Enqueue_Consume", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		queue := "compliance.enqueue_consume"
		want := []byte("hello-queue")

		var got []byte
		var wg sync.WaitGroup
		wg.Add(1)

		go func() {
			_ = consumer.Consume(ctx, queue, "g1", func(_ context.Context, msg *Message) error {
				got = msg.Data
				wg.Done()
				return nil
			})
		}()

		time.Sleep(20 * time.Millisecond) // let consumer register

		if err := producer.Enqueue(ctx, queue, want); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}

		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()

		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("timed out waiting for Consume")
		}

		if string(got) != string(want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("Publish_Subscribe", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		topic := "compliance.publish_subscribe"
		want := []byte("hello-topic")

		var received int32
		var wg sync.WaitGroup
		wg.Add(2) // two subscribers

		sub := func() {
			_ = consumer.Subscribe(ctx, topic, func(_ context.Context, msg *Message) error {
				if string(msg.Data) == string(want) {
					atomic.AddInt32(&received, 1)
					wg.Done()
				}
				return nil
			})
		}
		go sub()
		go sub()

		time.Sleep(20 * time.Millisecond)

		if err := producer.Publish(ctx, topic, want); err != nil {
			t.Fatalf("Publish: %v", err)
		}

		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()

		select {
		case <-done:
		case <-ctx.Done():
			t.Fatalf("timed out: only %d/2 subscribers received", atomic.LoadInt32(&received))
		}
	})

	t.Run("Nak_Redelivers", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		queue := "compliance.nak_redeliver"

		var attempts int32
		var wg sync.WaitGroup
		wg.Add(2) // expect two deliveries: first nak, second ack

		go func() {
			_ = consumer.Consume(ctx, queue, "g1", func(_ context.Context, _ *Message) error {
				n := atomic.AddInt32(&attempts, 1)
				wg.Done()
				if n == 1 {
					return context.DeadlineExceeded // trigger nak
				}
				return nil // second attempt: ack
			})
		}()

		time.Sleep(20 * time.Millisecond)

		if err := producer.Enqueue(ctx, queue, []byte("nak-test")); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}

		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()

		select {
		case <-done:
		case <-ctx.Done():
			t.Fatalf("timed out after %d attempts", atomic.LoadInt32(&attempts))
		}
	})

	t.Run("CompetingConsumers_SameGroup", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		queue := "compliance.competing"
		const msgCount = 4

		var total int32
		var wg sync.WaitGroup
		wg.Add(msgCount)

		startConsumer := func() {
			go func() {
				_ = consumer.Consume(ctx, queue, "workers", func(_ context.Context, _ *Message) error {
					atomic.AddInt32(&total, 1)
					wg.Done()
					return nil
				})
			}()
		}
		startConsumer()
		startConsumer()

		time.Sleep(20 * time.Millisecond)

		for i := range msgCount {
			if err := producer.Enqueue(ctx, queue, []byte("msg")); err != nil {
				t.Fatalf("Enqueue[%d]: %v", i, err)
			}
		}

		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()

		select {
		case <-done:
		case <-ctx.Done():
			t.Fatalf("timed out: processed %d/%d", atomic.LoadInt32(&total), msgCount)
		}

		if got := atomic.LoadInt32(&total); got != msgCount {
			t.Errorf("total = %d, want %d (each message delivered exactly once)", got, msgCount)
		}
	})

	t.Run("Close_IsGraceful", func(t *testing.T) {
		if err := producer.Close(); err != nil {
			t.Errorf("producer.Close() = %v", err)
		}
		if err := consumer.Close(); err != nil {
			t.Errorf("consumer.Close() = %v", err)
		}
	})
}
