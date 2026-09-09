package telemetry

import (
	"context"
	"testing"

	"github.com/google/uuid"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/httpclient"
)

func TestRequestIDGenerator_DerivesTraceIDFromUUIDRequestID(t *testing.T) {
	g := requestIDGenerator{}
	id := uuid.New()
	ctx := nexushttp.WithRequestID(context.Background(), id.String())

	tid, sid := g.NewIDs(ctx)

	if got := uuid.UUID(tid); got != id {
		t.Fatalf("trace id = %s, want it to equal the request UUID %s", got, id)
	}
	if !tid.IsValid() {
		t.Fatal("trace id is not valid")
	}
	if !sid.IsValid() {
		t.Fatal("span id is not valid")
	}
}

func TestRequestIDGenerator_RandomWhenNoRequestID(t *testing.T) {
	g := requestIDGenerator{}

	tid1, sid1 := g.NewIDs(context.Background())
	tid2, _ := g.NewIDs(context.Background())

	if !tid1.IsValid() || !sid1.IsValid() {
		t.Fatal("expected valid random ids when no request id is present")
	}
	if tid1 == tid2 {
		t.Fatal("expected distinct random trace ids across calls")
	}
}

func TestRequestIDGenerator_RandomWhenRequestIDNotUUID(t *testing.T) {
	g := requestIDGenerator{}
	// The agent's intercepted-flow ids and other non-UUID values must not
	// crash the generator; they fall back to a random valid trace id.
	ctx := nexushttp.WithRequestID(context.Background(), "192.168.1.5:54321-104.18.2.3:443-1716800000123")

	tid, sid := g.NewIDs(ctx)

	if !tid.IsValid() {
		t.Fatal("expected a valid random trace id for a non-UUID request id")
	}
	if !sid.IsValid() {
		t.Fatal("expected a valid span id")
	}
}

func TestRequestIDGenerator_NewSpanIDIsValidAndRandom(t *testing.T) {
	g := requestIDGenerator{}
	var tid = randomTraceID()

	sid1 := g.NewSpanID(context.Background(), tid)
	sid2 := g.NewSpanID(context.Background(), tid)

	if !sid1.IsValid() || !sid2.IsValid() {
		t.Fatal("span ids must be valid")
	}
	if sid1 == sid2 {
		t.Fatal("expected distinct random span ids")
	}
}
