package mq

import (
	"encoding/binary"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// TestBinwireFieldRegistryNoDrift keeps the binary field-id registry in lockstep
// with the wire struct. A TrafficEventMessage field added without a registered
// FieldID would be silently dropped by AppendBinary; this fails the build/CI the
// moment the json-tagged field count diverges from the registry length, forcing
// the author to register the id (and then handle it on both encode and decode).
func TestBinwireFieldRegistryNoDrift(t *testing.T) {
	want := countJSONTaggedFields(reflect.TypeOf(TrafficEventMessage{}))
	got := len(AllFieldIDs())
	if got != want {
		t.Fatalf("field-id registry drift: %d ids registered but TrafficEventMessage has %d json-tagged fields — add/remove a Fld* id (and its encode+decode handling)", got, want)
	}
}

// reservedFieldIDs are wire numbers deliberately left unassigned in THIS
// tree so a concurrently developed branch can land them without a collision.
// Empty now: 105/106 (artifact_refs / compliance_coverage) and 107/108
// (end_user_id / session_id) have all landed as Fld* constants — the two
// branches merged, each keeping its own contiguous ids.
var reservedFieldIDs = map[FieldID]bool{}

// TestBinwireFieldIDsUniqueAndContiguous locks the wire-number contract: ids
// are unique (no two fields share a wire number) and fill 1..max with no gap
// except the documented reservations (an undocumented gap is a typo'd wire
// number waiting to be silently reused). Both failure modes corrupt
// cross-version decode.
func TestBinwireFieldIDsUniqueAndContiguous(t *testing.T) {
	ids := AllFieldIDs()
	seen := make(map[FieldID]bool, len(ids))
	maxID := FieldID(0)
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate field id %d", id)
		}
		if reservedFieldIDs[id] {
			t.Fatalf("field id %d is reserved for a concurrent branch — allocate the next free number instead", id)
		}
		seen[id] = true
		if id > maxID {
			maxID = id
		}
	}
	for n := FieldID(1); n <= maxID; n++ {
		if !seen[n] && !reservedFieldIDs[n] {
			t.Fatalf("field id %d missing — ids must fill 1..%d except documented reservations", n, maxID)
		}
	}
}

// TestBinwireRoundTrip_RouterAndEmbeddingProviderFields proves AppendBinary
// serializes the three vendor-spend attribution fields (router_cost_usd,
// router_provider_id, embedding_provider_id) correctly at the byte level. The
// mq package owns only the ENCODE side of the wire codec (decode lives in
// nexus-hub's consumer package, an internal package this test cannot import),
// so this uses decodeTrafficEventForTest — a minimal local TLV walker scoped
// to exactly the field kinds a message built this way emits — rather than the
// production decoder. The real end-to-end round trip (encode here, decode via
// the actual Hub decoder) is TestDecodeTrafficEvent_RouterAndEmbeddingProviderFields
// in packages/nexus-hub/internal/observability/consumer.
func TestBinwireRoundTrip_RouterAndEmbeddingProviderFields(t *testing.T) {
	cost := 0.0066
	in := TrafficEventMessage{
		ID:                  "evt-1",
		Source:              "ai-gateway",
		RouterCostUsd:       &cost,
		RouterProviderID:    "prov-openai",
		EmbeddingProviderID: "prov-openai-embed",
	}

	out := decodeTrafficEventForTest(t, in.AppendBinary(nil))
	if out.RouterCostUsd == nil || *out.RouterCostUsd != cost {
		t.Errorf("RouterCostUsd = %v, want %v", out.RouterCostUsd, cost)
	}
	if out.RouterProviderID != "prov-openai" {
		t.Errorf("RouterProviderID = %q, want %q", out.RouterProviderID, "prov-openai")
	}
	if out.EmbeddingProviderID != "prov-openai-embed" {
		t.Errorf("EmbeddingProviderID = %q, want %q", out.EmbeddingProviderID, "prov-openai-embed")
	}
}

// decodedTrafficEventForTest holds the subset of decoded fields
// TestBinwireRoundTrip_RouterAndEmbeddingProviderFields asserts on.
type decodedTrafficEventForTest struct {
	RouterCostUsd       *float64
	RouterProviderID    string
	EmbeddingProviderID string
}

// decodeTrafficEventForTest walks one binary TLV record produced by
// AppendBinary, recognizing only the field-ids a message built like
// TestBinwireRoundTrip_RouterAndEmbeddingProviderFields's `in` emits
// (the always-on header, the three new vendor-spend fields, and the two
// always-on absent bodies). It fails the test on any other id rather than
// silently skip it, since skipping an unknown TLV value requires knowing its
// length, which this deliberately-minimal walker does not track for the full
// field registry — that full decode is the production Hub decoder's job
// (packages/nexus-hub/internal/observability/consumer/binwire_decode.go).
func decodeTrafficEventForTest(t *testing.T, data []byte) decodedTrafficEventForTest {
	t.Helper()
	var out decodedTrafficEventForTest
	n := 0
	for n < len(data) {
		id, idLen := binary.Uvarint(data[n:])
		if idLen <= 0 {
			t.Fatalf("bad field-id uvarint at offset %d", n)
		}
		n += idLen
		switch FieldID(id) {
		case FldID, FldSource:
			ln, lnLen := binary.Uvarint(data[n:])
			if lnLen <= 0 {
				t.Fatalf("bad length uvarint for field %d at offset %d", id, n)
			}
			n += lnLen + int(ln)
		case FldTimestamp, FldLatencyMs:
			_, vLen := binary.Varint(data[n:])
			if vLen <= 0 {
				t.Fatalf("bad varint for field %d at offset %d", id, n)
			}
			n += vLen
		case FldRouterCostUsd:
			if n+8 > len(data) {
				t.Fatalf("short read decoding FldRouterCostUsd")
			}
			v := math.Float64frombits(binary.LittleEndian.Uint64(data[n:]))
			out.RouterCostUsd = &v
			n += 8
		case FldRouterProviderID:
			ln, lnLen := binary.Uvarint(data[n:])
			if lnLen <= 0 {
				t.Fatalf("bad length uvarint for FldRouterProviderID at offset %d", n)
			}
			n += lnLen
			out.RouterProviderID = string(data[n : n+int(ln)])
			n += int(ln)
		case FldEmbeddingProviderID:
			ln, lnLen := binary.Uvarint(data[n:])
			if lnLen <= 0 {
				t.Fatalf("bad length uvarint for FldEmbeddingProviderID at offset %d", n)
			}
			n += lnLen
			out.EmbeddingProviderID = string(data[n : n+int(ln)])
			n += int(ln)
		case FldRequestBody, FldResponseBody:
			_, consumed, err := audit.ReadBodyBinary(data[n:])
			if err != nil {
				t.Fatalf("ReadBodyBinary: %v", err)
			}
			n += consumed
		default:
			t.Fatalf("unexpected field id %d in minimal test record (decodeTrafficEventForTest "+
				"only recognizes the fields this test's message construction emits)", id)
		}
	}
	return out
}

func countJSONTaggedFields(tp reflect.Type) int {
	n := 0
	for i := range tp.NumField() {
		tag := tp.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		if strings.Split(tag, ",")[0] == "" {
			continue
		}
		n++
	}
	return n
}
