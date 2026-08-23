package mq

// binwire_appenders.go — the low-level TLV value writers and the
// present-check wrappers that decide whether a field is written at all
// (mirroring the producer struct's `omitempty` / pointer-for-NULL semantics).
// Split out of binwire.go (which keeps the field-id registry and the
// AppendBinary method that calls these) to stay under the repo's per-file
// line cap; there is no behavioral seam here, purely a file-size one.

import (
	"encoding/binary"
	"math"

	"github.com/goccy/go-json"
)

// --- low-level value appenders (id-tagged). Value layout is implied by the id. ---

func putID(dst []byte, id FieldID) []byte { return binary.AppendUvarint(dst, uint64(id)) }

func aStr(dst []byte, id FieldID, s string) []byte {
	dst = putID(dst, id)
	dst = binary.AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

func aBytes(dst []byte, id FieldID, b []byte) []byte {
	dst = putID(dst, id)
	dst = binary.AppendUvarint(dst, uint64(len(b)))
	return append(dst, b...)
}

// aI64 writes a signed integer zig-zag varint (compact for small magnitudes).
func aI64(dst []byte, id FieldID, v int64) []byte {
	dst = putID(dst, id)
	return binary.AppendVarint(dst, v)
}

func aF64(dst []byte, id FieldID, f float64) []byte {
	dst = putID(dst, id)
	return binary.LittleEndian.AppendUint64(dst, math.Float64bits(f))
}

func aBool(dst []byte, id FieldID, v bool) []byte {
	dst = putID(dst, id)
	if v {
		return append(dst, 1)
	}
	return append(dst, 0)
}

func aTimeNanos(dst []byte, id FieldID, nanos int64) []byte {
	dst = putID(dst, id)
	return binary.AppendVarint(dst, nanos)
}

func aStrSlice(dst []byte, id FieldID, ss []string) []byte {
	dst = putID(dst, id)
	dst = binary.AppendUvarint(dst, uint64(len(ss)))
	for _, s := range ss {
		dst = binary.AppendUvarint(dst, uint64(len(s)))
		dst = append(dst, s...)
	}
	return dst
}

// aJSON writes a length-prefixed raw-JSON blob for the schema-opaque fields
// (Identity / *HooksPipeline / RoutingTrace / Details / *Normalized /
// *RedactionSpans / *BlockingRule / InternalOpsBreakdown / LatencyBreakdown).
// The decoder stores these back verbatim as json.RawMessage.
func aJSON(dst []byte, id FieldID, raw []byte) []byte { return aBytes(dst, id, raw) }

// --- present-check wrappers (keep AppendBinary readable) ---

func optStr(dst []byte, id FieldID, s string) []byte {
	if s == "" {
		return dst
	}
	return aStr(dst, id, s)
}

func optPtrStr(dst []byte, id FieldID, p *string) []byte {
	if p == nil {
		return dst
	}
	return aStr(dst, id, *p)
}

func optI64(dst []byte, id FieldID, v int64) []byte {
	if v == 0 {
		return dst
	}
	return aI64(dst, id, v)
}

func optPtrI64(dst []byte, id FieldID, p *int64) []byte {
	if p == nil {
		return dst
	}
	return aI64(dst, id, *p)
}

func optPtrIntAsI64(dst []byte, id FieldID, p *int) []byte {
	if p == nil {
		return dst
	}
	return aI64(dst, id, int64(*p))
}

func optF64(dst []byte, id FieldID, f float64) []byte {
	if f == 0 {
		return dst
	}
	return aF64(dst, id, f)
}

func optPtrF64(dst []byte, id FieldID, p *float64) []byte {
	if p == nil {
		return dst
	}
	return aF64(dst, id, *p)
}

func optRaw(dst []byte, id FieldID, raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return dst
	}
	return aJSON(dst, id, raw)
}

func optPtrRaw(dst []byte, id FieldID, p *json.RawMessage) []byte {
	if p == nil || len(*p) == 0 {
		return dst
	}
	return aJSON(dst, id, *p)
}

// optJSONValue marshals a schema-opaque value (map/any) and writes it as a JSON
// blob when it is non-nil and not the literal null. A marshal error drops the
// field (it would have produced an unreadable column anyway); audit never fails
// the surrounding record over one opaque side-field.
func optJSONValue(dst []byte, id FieldID, v any) []byte {
	if v == nil {
		return dst
	}
	raw, err := json.Marshal(v)
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return dst
	}
	return aJSON(dst, id, raw)
}
