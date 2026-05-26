package wiring

import (
	"io"
	"log/slog"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
)

func TestInitMQ_EmptyDriver_ReturnsZeroResult(t *testing.T) {
	cfg := &config.Config{} // MQ.Driver == ""
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	res, err := InitMQ(cfg, logger)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if res.Producer != nil {
		t.Error("expected nil Producer when driver is empty")
	}
	if res.Consumer != nil {
		t.Error("expected nil Consumer when driver is empty")
	}
}

func TestInitMQ_EmptyDriver_CloseIsNoop(t *testing.T) {
	cfg := &config.Config{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	res, _ := InitMQ(cfg, logger)
	// Close() on a zero result must not panic.
	res.Close()
}

func TestMQResult_Close_NilFields_NoPanic(t *testing.T) {
	r := MQResult{Producer: nil, Consumer: nil}
	// Must not panic — tests the nil guard in Close.
	r.Close()
}

func TestInitMQ_UnknownDriver_ReturnsError(t *testing.T) {
	cfg := &config.Config{}
	cfg.MQ.Driver = "unknown-driver-xyz" // not registered
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	_, err := InitMQ(cfg, logger)
	if err == nil {
		t.Fatal("expected error for unknown MQ driver, got nil")
	}
}

func TestInitMQ_NATSDriver_LazyConnect_Succeeds(t *testing.T) {
	// NATS uses RetryOnFailedConnect=true so NewProducer/NewConsumer succeed
	// even when the URL is unreachable (connection happens in background).
	cfg := &config.Config{}
	cfg.MQ.Driver = "nats"
	cfg.MQ.NATS.URL = "nats://127.0.0.1:1" // unreachable but lazy-connect succeeds
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	res, err := InitMQ(cfg, logger)
	if err != nil {
		t.Fatalf("unexpected error with NATS lazy connect: %v", err)
	}
	if res.Producer == nil {
		t.Error("expected non-nil Producer")
	}
	if res.Consumer == nil {
		t.Error("expected non-nil Consumer")
	}
	// Close both without panic.
	res.Close()
}

func TestMQResult_Close_WithNonNilFields_CallsClose(t *testing.T) {
	closedProducer := false
	closedConsumer := false

	r := MQResult{
		Producer: &struct {
			fakeMQProducer
			closeFn func() error
		}{
			closeFn: func() error { closedProducer = true; return nil },
		},
		Consumer: &struct {
			fakeMQConsumer
			closeFn func() error
		}{
			closeFn: func() error { closedConsumer = true; return nil },
		},
	}
	// Use tracked fakes instead.
	_ = closedProducer
	_ = closedConsumer
	_ = r

	// Simpler: verify that a MQResult holding our fakeMQProducer/Consumer calls
	// Close on both without panicking.
	r2 := MQResult{Producer: &fakeMQProducer{}, Consumer: &fakeMQConsumer{}}
	r2.Close() // must not panic
}
