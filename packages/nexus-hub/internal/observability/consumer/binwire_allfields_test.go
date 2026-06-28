package consumer

import (
	"reflect"
	"testing"
	"time"

	json "github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// TestBinwireAllFieldsRoundTrip populates EVERY field of the producer message with
// a non-zero value, encodes it, and decodes it — catching any field-id whose
// encode type disagrees with its decode type (which desyncs the TLV stream the
// moment that field appears in real traffic, the class of bug the sparse
// round-trip cases missed). It asserts: (1) decode does not error, (2) the whole
// record is consumed (no trailing bytes → no mid-record desync), (3) the
// always-present scalars and a sample of pointer fields survive.
func TestBinwireAllFieldsRoundTrip(t *testing.T) {
	var m mq.TrafficEventMessage
	populateAllFields(reflect.ValueOf(&m).Elem())
	m.ID = "evt-all"
	m.Source = "ai-gateway"
	m.Timestamp = time.Unix(1_700_000_000, 0).UTC()

	rec := m.AppendBinary(nil)
	got, err := decodeBinaryRecord(rec)
	if err != nil {
		t.Fatalf("decode error on fully-populated record: %v", err)
	}
	if got.ID != "evt-all" || got.Source != "ai-gateway" {
		t.Fatalf("scalar header mangled: id=%q source=%q (field-id desync)", got.ID, got.Source)
	}
	// Every consumer pointer field must be non-nil (all source fields were set).
	// A nil here means that field-id was not written, or a preceding field-id
	// desynced the stream so this one was never reached.
	cv := reflect.ValueOf(got)
	ct := cv.Type()
	for i := 0; i < cv.NumField(); i++ {
		f := cv.Field(i)
		name := ct.Field(i).Name
		if f.Kind() == reflect.Ptr && f.IsNil() {
			t.Errorf("consumer field %s is nil after decode of a fully-populated record (field-id desync or unmapped id)", name)
		}
	}

	// Value-typed (non-pointer) string fields are invisible to the pointer-nil
	// check above, yet an encode-case or decode-case omission for one silently
	// drops it on the wire while the field counts stay balanced. Assert every
	// consumer value-string field that has a populated producer counterpart (same
	// name, set to non-empty by populateAllFields) survived the round-trip.
	pv := reflect.ValueOf(m)
	pt := pv.Type()
	producerStr := map[string]bool{}
	for i := 0; i < pv.NumField(); i++ {
		if pt.Field(i).Type.Kind() == reflect.String && pv.Field(i).String() != "" {
			producerStr[pt.Field(i).Name] = true
		}
	}
	for i := 0; i < cv.NumField(); i++ {
		f := cv.Field(i)
		name := ct.Field(i).Name
		if f.Kind() == reflect.String && producerStr[name] && f.String() == "" {
			t.Errorf("consumer value-string field %s is empty after decode of a fully-populated record (encode/decode omission or field-id desync)", name)
		}
	}
}

// populateAllFields sets every settable field of v to a distinctive non-zero value
// so the encoder writes EVERY field-id.
func populateAllFields(v reflect.Value) {
	rawType := reflect.TypeOf(json.RawMessage(nil))
	tp := v.Type()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if !f.CanSet() {
			continue
		}
		ft := tp.Field(i).Type
		switch {
		case ft == rawType:
			f.Set(reflect.ValueOf(json.RawMessage(`{"k":1}`)))
		case ft == reflect.TypeOf(&json.RawMessage{}):
			rm := json.RawMessage(`{"k":1}`)
			f.Set(reflect.ValueOf(&rm))
		case ft.Kind() == reflect.String:
			f.SetString("x")
		case ft.Kind() == reflect.Int || ft.Kind() == reflect.Int64:
			f.SetInt(7)
		case ft.Kind() == reflect.Float64:
			f.SetFloat(1.5)
		case ft.Kind() == reflect.Bool:
			f.SetBool(true)
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.String:
			f.Set(reflect.ValueOf([]string{"a", "b"}))
		case ft.Kind() == reflect.Map:
			// map[string]any / map[string]int
			mv := reflect.MakeMap(ft)
			mv.SetMapIndex(reflect.ValueOf("k"), reflect.New(ft.Elem()).Elem())
			f.Set(mv)
		case ft.Kind() == reflect.Ptr:
			f.Set(reflect.New(ft.Elem()))
			pe := f.Elem()
			switch pe.Kind() {
			case reflect.String:
				pe.SetString("p")
			case reflect.Int, reflect.Int64:
				pe.SetInt(9)
			case reflect.Float64:
				pe.SetFloat(2.5)
			case reflect.Bool:
				pe.SetBool(true)
			}
		case ft.Kind() == reflect.Interface:
			// `any` fields (hooks pipeline / routing trace / details)
			f.Set(reflect.ValueOf(json.RawMessage(`{"a":1}`)))
		case ft.Kind() == reflect.Struct:
			// time.Time / audit.Body — leave to caller (Timestamp set above; bodies
			// default to absent which is a valid encode).
		}
	}
}
