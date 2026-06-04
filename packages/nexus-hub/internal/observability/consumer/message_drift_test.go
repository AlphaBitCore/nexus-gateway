package consumer

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// jsonTagSet returns the set of JSON field names a struct declares, stripping
// ",omitempty" and skipping untagged / json:"-" fields. Only the wire NAME
// matters here — the Go types deliberately differ between the two structs.
func jsonTagSet(t reflect.Type) map[string]struct{} {
	out := make(map[string]struct{}, t.NumField())
	for i := range t.NumField() {
		raw := t.Field(i).Tag.Get("json")
		if raw == "" {
			continue
		}
		name := strings.Split(raw, ",")[0]
		if name == "" || name == "-" {
			continue
		}
		out[name] = struct{}{}
	}
	return out
}

func sortedDiff(a, b map[string]struct{}) []string {
	var only []string
	for k := range a {
		if _, ok := b[k]; !ok {
			only = append(only, k)
		}
	}
	sort.Strings(only)
	return only
}

// TestTrafficEventMessage_NoStructDrift enforces the load-bearing wire
// contract between the two TrafficEventMessage structs:
//
//   - mq.TrafficEventMessage           — the producer/wire format published
//     by ai-gateway, compliance-proxy, and agent (value types + omitempty,
//     map/any for JSONB blobs — optimized for marshalling from live objects).
//   - consumer.TrafficEventMessage     — the Hub deserialization struct
//     (pointer types so absent → SQL NULL, json.RawMessage for JSONB
//     passthrough — optimized for unmarshal + persist).
//
// The two are intentionally separate DTOs (each optimized for its direction),
// so they cannot share a single Go type without imposing conversion cost on
// one side. What they MUST share is the JSON tag set: a field added to the
// producer but not the consumer is silently dropped on unmarshal and never
// reaches traffic_event. That is exactly the endpoint_type incident this test
// exists to prevent from recurring.
//
// (A further "consumer field → traffic_event INSERT column" guard is NOT
// asserted here: many consumer fields land on traffic_event_payload /
// traffic_event_normalized or are merged into JSONB, so a tag⊆columns check
// is too noisy to be reliable. That producer→consumer→DB round-trip is
// covered end-to-end by the ai-gateway smoke.)
func TestTrafficEventMessage_NoStructDrift(t *testing.T) {
	producer := jsonTagSet(reflect.TypeOf(mq.TrafficEventMessage{}))
	consumer := jsonTagSet(reflect.TypeOf(TrafficEventMessage{}))

	if droppedByHub := sortedDiff(producer, consumer); len(droppedByHub) > 0 {
		t.Errorf("mq.TrafficEventMessage publishes JSON tags missing from "+
			"consumer.TrafficEventMessage — the Hub SILENTLY DROPS these fields on "+
			"unmarshal: %v\n"+
			"Fix: add each field to packages/nexus-hub/internal/observability/consumer/"+
			"message.go AND persist it in the traffic.go INSERT (+ schema.prisma + a "+
			"migration for the new column).", droppedByHub)
	}

	if unsent := sortedDiff(consumer, producer); len(unsent) > 0 {
		t.Errorf("consumer.TrafficEventMessage declares JSON tags that no producer "+
			"sends: %v\n"+
			"Fix: remove them from the consumer struct, or add them to "+
			"packages/shared/transport/mq/messages.go if a producer should emit them.", unsent)
	}
}
