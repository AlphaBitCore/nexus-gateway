package consumer

import (
	"reflect"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// TestDecodeAlertView_ParityWithFullDecode is the drift guard for the narrow
// alert decode. It populates EVERY producer field, encodes one record, then
// decodes it twice: the full alert decode (decodeBinaryRecordInto, skipBody=true)
// and the narrow view decode (decodeBinaryRecordIntoView). Every AlertView field
// must equal the full decode's same-named field.
//
// This catches the failure mode the narrowing introduces: skipField must advance
// past each of the ~80 non-view fields using that field's exact wire kind
// (varint / f64 / bool / strSlice / length-prefixed bytes). If a field's kind is
// misclassified — e.g. a new varint field added to the producer but not to
// skipField's varint list — the byte cursor desyncs the moment that field appears,
// so a later view field reads the wrong bytes (value mismatch here) or the stream
// over/under-reads (decode error here). Either way this test fails loudly instead
// of the alert engine silently reading garbage in production.
func TestDecodeAlertView_ParityWithFullDecode(t *testing.T) {
	var m mq.TrafficEventMessage
	populateAllFields(reflect.ValueOf(&m).Elem())
	m.ID = "evt-all"
	m.Source = "ai-gateway"
	m.Timestamp = time.Unix(1_700_000_000, 0).UTC()
	rec := m.AppendBinary(nil)

	// Full decode with the alert body contract (skipBody=true) so body fields are
	// compared metadata-only on both sides.
	var full TrafficEventMessage
	if err := decodeBinaryRecordInto(&full, rec, true); err != nil {
		t.Fatalf("full decode of fully-populated record: %v", err)
	}

	var view AlertView
	if err := decodeBinaryRecordIntoView(&view, rec); err != nil {
		t.Fatalf("view decode of fully-populated record: %v (skipField likely mis-advanced a field)", err)
	}

	// Compare every AlertView field to the full struct's same-named field. The
	// field names are identical by construction, so reflection keeps this in
	// lockstep with the struct without hand-listing all 22.
	vv := reflect.ValueOf(view)
	vt := vv.Type()
	fv := reflect.ValueOf(full)
	for i := 0; i < vv.NumField(); i++ {
		name := vt.Field(i).Name
		ff := fv.FieldByName(name)
		if !ff.IsValid() {
			t.Fatalf("AlertView field %s has no TrafficEventMessage counterpart — field-name drift", name)
		}
		got := vv.Field(i).Interface()
		want := ff.Interface()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("field %s: view=%#v full=%#v (skipField desync or mismapped store case)", name, got, want)
		}
	}
}

// TestDecodeAlertView_SkipsNonViewFields confirms the view decode does NOT pay for
// the large non-view json fields: a record whose only populated heavy field is the
// kilobyte-scale ResponseNormalized decodes into an AlertView that holds none of
// it, and decoding allocates far less than the full decode (which copies it).
func TestDecodeAlertView_SkipsNonViewFields(t *testing.T) {
	big := make([]byte, 8*1024)
	for i := range big {
		big[i] = byte('a' + i%26)
	}
	bigJSON := append(append([]byte(`{"text":"`), big...), []byte(`"}`)...)
	ts := time.Unix(1_700_000_000, 0).UTC()
	ent := "vk-1"
	m := mq.TrafficEventMessage{
		ID: "evt-1", Source: "ai-gateway", Timestamp: ts, EntityID: ent,
		ResponseNormalized: bigJSON, // ~8 KB, not a view field
		RequestNormalized:  bigJSON, // ~8 KB, not a view field
	}
	rec := m.AppendBinary(nil)

	var view AlertView
	if err := decodeBinaryRecordIntoView(&view, rec); err != nil {
		t.Fatalf("view decode: %v", err)
	}
	if view.EntityID == nil || *view.EntityID != ent {
		t.Fatalf("EntityID = %v, want %q (view field must survive past skipped heavy fields)", view.EntityID, ent)
	}

	// The narrow decode must allocate dramatically less than the full decode, which
	// copies both ~8 KB normalized blobs. allocsPerRun is integral; the full decode
	// copies ≥16 KB of normalized json the view never touches.
	fullBytes := testing.AllocsPerRun(50, func() {
		var f TrafficEventMessage
		_ = decodeBinaryRecordInto(&f, rec, true)
	})
	viewBytes := testing.AllocsPerRun(50, func() {
		var v AlertView
		_ = decodeBinaryRecordIntoView(&v, rec)
	})
	if viewBytes >= fullBytes {
		t.Errorf("view decode allocs (%v) not fewer than full decode allocs (%v) — heavy non-view fields not skipped", viewBytes, fullBytes)
	}
}
