package audit

import (
	"bytes"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"testing"
	"time"
)

// TestMarshalChunkParallel_ByteIdenticalToSerial locks the parallel flush
// marshal: marshalChunkParallel must produce exactly the bytes a serial
// per-record marshalRecord would, in input order, for every record. The
// audit wire contract is byte-for-byte; parallelism is a throughput change
// only, never an output change.
func TestMarshalChunkParallel_ByteIdenticalToSerial(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default()).
		WithNormalizer(func(direction, _, _, _, _ string, _ bool, body []byte) (json.RawMessage, string, string) {
			return json.RawMessage(`{"d":"` + direction + `","n":` + fmt.Sprint(len(body)) + `}`), "ok", ""
		})

	batch := make([]*Record, 0, 200)
	for i := range 200 {
		batch = append(batch, &Record{
			RequestID:    fmt.Sprintf("req-%d", i),
			Timestamp:    time.Unix(int64(1700000000+i), 0).UTC(),
			RequestBody:  []byte(fmt.Sprintf(`{"model":"m","i":%d,"pad":"%s"}`, i, bytes.Repeat([]byte("x"), i*7))),
			ResponseBody: []byte(fmt.Sprintf(`{"choices":[%d]}`, i)),
			ModelName:    "m",
			Path:         "/v1/chat/completions",
		})
	}

	// Serial reference.
	wantD := make([][]byte, 0, len(batch))
	wantR := make([]*Record, 0, len(batch))
	for _, rec := range batch {
		if d, _, ok := w.marshalRecord(rec); ok {
			wantD = append(wantD, d)
			wantR = append(wantR, rec)
		}
	}

	gotD, _, gotR := w.marshalChunkParallel(batch)

	if len(gotD) != len(wantD) {
		t.Fatalf("parallel produced %d records, serial %d", len(gotD), len(wantD))
	}
	for i := range wantD {
		if gotR[i] != wantR[i] {
			t.Fatalf("record %d order mismatch: got %s want %s", i, gotR[i].RequestID, wantR[i].RequestID)
		}
		if !bytes.Equal(gotD[i], wantD[i]) {
			t.Fatalf("record %d bytes differ:\n parallel=%s\n serial  =%s", i, gotD[i], wantD[i])
		}
	}
}

// TestMarshalChunkParallel_EmptyAndSingle covers the n==0 and n==1 fast paths.
func TestMarshalChunkParallel_EmptyAndSingle(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default()).
		WithNormalizer(func(_, _, _, _, _ string, _ bool, _ []byte) (json.RawMessage, string, string) {
			return json.RawMessage(`{}`), "ok", ""
		})
	if d, b, r := w.marshalChunkParallel(nil); d != nil || b != nil || r != nil {
		t.Fatalf("empty chunk must return nil,nil,nil; got %v,%v,%v", d, b, r)
	}
	one := []*Record{{RequestID: "solo", Timestamp: time.Now(), RequestBody: []byte(`{"model":"m"}`)}}
	d, _, r := w.marshalChunkParallel(one)
	if len(d) != 1 || len(r) != 1 || r[0].RequestID != "solo" {
		t.Fatalf("single chunk mismatch: d=%d r=%v", len(d), r)
	}
	wantD, _, _ := w.marshalRecord(one[0])
	if !bytes.Equal(d[0], wantD) {
		t.Fatalf("single-record bytes differ from serial")
	}
}
