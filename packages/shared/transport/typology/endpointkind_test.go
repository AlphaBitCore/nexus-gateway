package typology

import "testing"

// TestEndpointKindConstants pins the canonical wire-format string for every
// EndpointKind constant. These strings are persisted into traffic_event,
// embedded in MQ payloads, and used as Prometheus label values — renaming
// any of them is a coordinated breaking change. Failing this test means
// somebody changed a constant value without updating the migration / wire
// contract.
func TestEndpointKindConstants(t *testing.T) {
	cases := []struct {
		k    EndpointKind
		want string
	}{
		{EndpointKindChat, "chat"},
		{EndpointKindEmbeddings, "embeddings"},
		{EndpointKindImageGeneration, "image_generation"},
		{EndpointKindTTS, "tts"},
		{EndpointKindSTT, "stt"},
		{EndpointKindVideoGeneration, "video_generation"},
		{EndpointKindBatch, "batch"},
		{EndpointKindJob, "job"},
		{EndpointKindModels, "models"},
	}
	for _, c := range cases {
		if string(c.k) != c.want {
			t.Errorf("EndpointKind %v string = %q, want %q", c.k, string(c.k), c.want)
		}
		if c.k.String() != c.want {
			t.Errorf("(EndpointKind).String() = %q, want %q", c.k.String(), c.want)
		}
	}
}

// TestAllEndpointKindsExhaustive verifies the AllEndpointKinds slice
// matches the count + identity of constants defined in endpointkind.go.
// Adding a new constant requires appending to AllEndpointKinds; this
// test catches the forgotten-append failure mode.
func TestAllEndpointKindsExhaustive(t *testing.T) {
	want := []EndpointKind{
		EndpointKindChat,
		EndpointKindEmbeddings,
		EndpointKindImageGeneration,
		EndpointKindTTS,
		EndpointKindSTT,
		EndpointKindVideoGeneration,
		EndpointKindBatch,
		EndpointKindJob,
		EndpointKindModels,
	}
	if len(AllEndpointKinds) != len(want) {
		t.Fatalf("len(AllEndpointKinds) = %d, want %d", len(AllEndpointKinds), len(want))
	}
	for i, k := range want {
		if AllEndpointKinds[i] != k {
			t.Errorf("AllEndpointKinds[%d] = %v, want %v", i, AllEndpointKinds[i], k)
		}
	}
}

func TestEndpointKind_IsValid(t *testing.T) {
	for _, k := range AllEndpointKinds {
		if !k.IsValid() {
			t.Errorf("IsValid(%v) = false, want true for defined constant", k)
		}
	}
	// Empty string is NOT a valid EndpointKind — callers needing
	// "unclassified" semantics check for "" separately.
	if EndpointKind("").IsValid() {
		t.Errorf("IsValid(\"\") = true, want false (empty is unclassified, not valid)")
	}
	// Random non-defined string is invalid.
	if EndpointKind("bogus").IsValid() {
		t.Errorf("IsValid(\"bogus\") = true, want false")
	}
}
