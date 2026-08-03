package codecs

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/goccy/go-json"
)

// decodeLenient unmarshals raw into dst (a pointer to struct). When the
// whole-struct decode fails on a type-level mismatch, it falls back to a
// field-by-field decode via map[string]json.RawMessage so ONE mistyped
// optional scalar (e.g. "temperature":"0.7" as a string) cannot erase the
// entire request from the normalized record — these bytes are captured
// third-party traffic, not something the gateway validated, and the audit
// record must preserve the message content it CAN read.
//
// Fields that fail their individual decode are left at their zero value.
// The error returned is nil when the fallback recovered at least one field;
// the original whole-struct error is returned only when raw is not a JSON
// object at all (nothing recoverable).
func decodeLenient(raw []byte, dst any) error {
	err := json.Unmarshal(raw, dst)
	if err == nil {
		return nil
	}
	var fields map[string]json.RawMessage
	if mapErr := json.Unmarshal(raw, &fields); mapErr != nil {
		// Not a JSON object — nothing to recover; surface the original error.
		return err
	}

	rv := reflect.ValueOf(dst).Elem()
	rt := rv.Type()
	recovered := 0
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			name = f.Name
		}
		v, ok := fields[name]
		if !ok || len(v) == 0 || string(v) == "null" {
			continue
		}
		fieldPtr := rv.Field(i).Addr().Interface()
		if ferrI := json.Unmarshal(v, fieldPtr); ferrI == nil {
			recovered++
		}
		// A field that fails its individual decode stays zero — the
		// mistyped scalar is dropped, everything else survives.
	}
	if recovered == 0 {
		return fmt.Errorf("lenient decode recovered no fields: %w", err)
	}
	return nil
}
