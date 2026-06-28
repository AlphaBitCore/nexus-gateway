package runtimeintrospect

import (
	"context"
	"github.com/goccy/go-json"
	"sync"
	"testing"
)

func TestKeyStateRecorder_RecordAndSource(t *testing.T) {
	r := NewKeyStateRecorder()
	r.Record("log_level", json.RawMessage(`{"level":"debug"}`))

	src := r.Source("log_level")
	if src.Name() != "config.log_level" {
		t.Fatalf("name = %q, want config.log_level", src.Name())
	}
	v, err := src.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot error: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("snapshot type = %T, want map[string]any", v)
	}
	if m["level"] != "debug" {
		t.Errorf("level = %v, want debug", m["level"])
	}
}

func TestKeyStateRecorder_NilForUnknownKey(t *testing.T) {
	r := NewKeyStateRecorder()
	v, err := r.Source("missing").Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot error: %v", err)
	}
	if v != nil {
		t.Errorf("snapshot = %v, want nil", v)
	}
}

func TestKeyStateRecorder_EmptyBytesClears(t *testing.T) {
	r := NewKeyStateRecorder()
	r.Record("k", json.RawMessage(`"v"`))
	r.Record("k", nil)
	v, _ := r.Source("k").Snapshot(context.Background())
	if v != nil {
		t.Errorf("after clear: snapshot = %v, want nil", v)
	}
}

func TestKeyStateRecorder_InvalidJSONFallback(t *testing.T) {
	r := NewKeyStateRecorder()
	r.Record("k", json.RawMessage(`not-json`))
	v, err := r.Source("k").Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot error: %v", err)
	}
	s, ok := v.(string)
	if !ok || s != "not-json" {
		t.Errorf("snapshot = %v (%T), want raw string fallback", v, v)
	}
}

func TestKeyStateRecorder_RecordCopiesBytes(t *testing.T) {
	r := NewKeyStateRecorder()
	src := []byte(`{"a":1}`)
	r.Record("k", src)
	src[0] = 'X' // mutate caller buffer after recording
	v, err := r.Source("k").Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot error: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("snapshot type = %T, want map[string]any (the mutation must not poison the recorded copy)", v)
	}
	if m["a"] != float64(1) {
		t.Errorf("a = %v, want 1", m["a"])
	}
}

func TestKeyStateRecorder_ConcurrentReadWrite(t *testing.T) {
	r := NewKeyStateRecorder()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 1000 {
			r.Record("k", json.RawMessage(`"v"`))
		}
	}()
	go func() {
		defer wg.Done()
		src := r.Source("k")
		for range 1000 {
			_, _ = src.Snapshot(context.Background())
		}
	}()
	wg.Wait()
}

func TestKeyStateRecorder_RegisterAll(t *testing.T) {
	reg := New("svc", "thing-1", "v0")
	r := NewKeyStateRecorder()
	r.Record("a", json.RawMessage(`1`))
	r.Record("b", json.RawMessage(`2`))
	r.RegisterAll(reg, []string{"a", "b", "c"}) // c never recorded; should still register and snapshot nil

	names := reg.Names()
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	for _, want := range []string{"config.a", "config.b", "config.c"} {
		if !have[want] {
			t.Errorf("missing source %q (have %v)", want, names)
		}
	}

	snap := reg.Snapshot(context.Background())
	if snap.Sources["config.a"].Value != float64(1) {
		t.Errorf("config.a = %v, want 1", snap.Sources["config.a"].Value)
	}
	if snap.Sources["config.c"].Value != nil {
		t.Errorf("config.c = %v, want nil (never recorded)", snap.Sources["config.c"].Value)
	}
}

func TestKeyStateRecorder_NilSafety(t *testing.T) {
	var r *KeyStateRecorder
	r.Record("k", json.RawMessage(`1`)) // should not panic
	r.RegisterAll(nil, []string{"k"})   // should not panic
}
