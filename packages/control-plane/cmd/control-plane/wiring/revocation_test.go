package wiring

import (
	"io"
	"log/slog"
	"testing"

	pgxmock "github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
)

func TestInitRevocation_NilDB_ReturnsZeroResult(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	res := InitRevocation(nil, nil, logger)
	if res.Store != nil {
		t.Error("expected nil Store when db is nil")
	}
	if res.Service != nil {
		t.Error("expected nil Service when db is nil")
	}
}

func TestInitRevocation_NilMQProducer_ReturnsZeroResult(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	db := store.NewWithPgxPool(mock)
	res := InitRevocation(db, nil, logger)
	if res.Store != nil {
		t.Error("expected nil Store when mqProducer is nil")
	}
	if res.Service != nil {
		t.Error("expected nil Service when mqProducer is nil")
	}
}

func TestInitRevocation_WithDBAndProducer_ReturnsPopulatedResult(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	db := store.NewWithPgxPool(mock)
	// Use a non-nil fake producer (nil-safe producer via audit pattern).
	fakeProducer := &fakeMQProducer{}
	res := InitRevocation(db, fakeProducer, logger)
	if res.Store == nil {
		t.Error("expected non-nil Store")
	}
	if res.Service == nil {
		t.Error("expected non-nil Service")
	}
}
