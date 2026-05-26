package wiring

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
)

func TestInitMQProducer_NoDriverReturnsNil(t *testing.T) {
	cfg := &config.Config{}
	p, err := InitMQProducer(cfg, testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != nil {
		t.Error("expected nil producer when MQ driver not configured")
	}
}

func TestInitMQProducer_UnknownDriverReturnsError(t *testing.T) {
	cfg := &config.Config{}
	cfg.MQ.Driver = "nonexistent-mq-driver"
	_, err := InitMQProducer(cfg, testLogger())
	if err == nil {
		t.Error("expected error for unknown MQ driver")
	}
}
