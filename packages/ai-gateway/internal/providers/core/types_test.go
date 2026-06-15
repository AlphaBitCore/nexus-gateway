package core

import (
	"bytes"
	"encoding/json"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"reflect"
	"strings"
	"testing"
)

// TestLimitedReadAllN pins the contract that the runtime variant uses the
// supplied cap, falls back to ReadAllLimit on a non-positive cap (so a
// zeroed payload-capture row never collapses the read), clamps an oversize
// body to the cap, and — the F-0349 contract — reports truncated=true
// exactly when the body exceeded the cap so usage extraction can refuse to
// claim "ok" over an incomplete buffer.
func TestLimitedReadAllN(t *testing.T) {
	body := bytes.Repeat([]byte("a"), 5000)

	cases := []struct {
		name          string
		cap           int64
		wantLen       int
		wantTruncated bool
	}{
		{"cap higher than body returns full body, not truncated", 10000, 5000, false},
		{"cap equal to body returns full body, not truncated", 5000, 5000, false},
		{"cap lower than body clamps and reports truncated", 1024, 1024, true},
		{"cap one below body reports truncated", 4999, 4999, true},
		{"zero cap falls back to ReadAllLimit, not truncated", 0, 5000, false},
		{"negative cap falls back to ReadAllLimit, not truncated", -1, 5000, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, truncated, err := LimitedReadAllN(bytes.NewReader(body), tc.cap)
			if err != nil {
				t.Fatalf("LimitedReadAllN: %v", err)
			}
			if len(got) != tc.wantLen {
				t.Errorf("len(got): want %d, got %d", tc.wantLen, len(got))
			}
			if truncated != tc.wantTruncated {
				t.Errorf("truncated: want %v, got %v", tc.wantTruncated, truncated)
			}
		})
	}
}

func TestFormat_Valid(t *testing.T) {
	for _, f := range AllFormats() {
		if !f.Valid() {
			t.Errorf("%s: expected valid", f)
		}
	}
	if Format("unknown").Valid() {
		t.Error("unknown format reported valid")
	}
	if Format("").Valid() {
		t.Error("empty format reported valid")
	}
}

// TestFormatOpenAIResponses_Valid pins that the Responses-API ingress format is
// detectable at the route layer (Valid() returns true) but is NOT in the
// chat-routing matrix (AllFormats() omits it on purpose, see types.go).
func TestFormatOpenAIResponses_Valid(t *testing.T) {
	if !FormatOpenAIResponses.Valid() {
		t.Fatal("FormatOpenAIResponses must report Valid for route detection")
	}
	for _, f := range AllFormats() {
		if f == FormatOpenAIResponses {
			t.Fatal("FormatOpenAIResponses must NOT appear in AllFormats() (chat-routing matrix); it serves EndpointResponsesAPI, not EndpointChatCompletions")
		}
	}
	// IsOpenAIFamily is also intentionally false — the Responses wire
	// body shape (input/output/instructions) is not interchangeable with
	// the chat-completions body shape (messages/choices), so simple
	// `payload["model"] = X` passthrough rewrites would corrupt it.
	if FormatOpenAIResponses.IsOpenAIFamily() {
		t.Error("FormatOpenAIResponses must NOT report IsOpenAIFamily — its body shape is incompatible with chat-completions passthrough rewrites")
	}
}

// TestAdapterSpec_SupportsShape pins the default-shape behavior so an
// adapter with no explicit RequestShapes declaration keeps reporting
// chat-completions support and nothing else.
func TestAdapterSpec_SupportsShape(t *testing.T) {
	defaultSpec := AdapterSpec{}
	if !defaultSpec.SupportsShape(typology.WireShapeOpenAIChat) {
		t.Error("empty AdapterSpec.RequestShapes must default to supporting chat-completions")
	}
	if defaultSpec.SupportsShape(typology.WireShapeOpenAIResponses) {
		t.Error("empty AdapterSpec.RequestShapes must NOT default to supporting responses-api")
	}
	explicitSpec := AdapterSpec{RequestShapes: []typology.WireShape{typology.WireShapeOpenAIChat, typology.WireShapeOpenAIResponses}}
	if !explicitSpec.SupportsShape(typology.WireShapeOpenAIChat) || !explicitSpec.SupportsShape(typology.WireShapeOpenAIResponses) {
		t.Error("explicit RequestShapes must match each declared shape")
	}
	if explicitSpec.SupportsShape(typology.WireShape("unknown-shape")) {
		t.Error("explicit RequestShapes must reject undeclared shapes")
	}
}

func TestCallTarget_Get(t *testing.T) {
	var empty CallTarget
	if got := empty.Get("x"); got != "" {
		t.Errorf("empty target: expected \"\", got %q", got)
	}

	target := CallTarget{Extras: map[string]string{"azure.apiVersion": "2024-02-15"}}
	if got := target.Get("azure.apiVersion"); got != "2024-02-15" {
		t.Errorf("expected api version, got %q", got)
	}
	if got := target.Get("missing"); got != "" {
		t.Errorf("missing key: expected \"\", got %q", got)
	}
}

func TestProviderError_Error(t *testing.T) {
	var nilErr *ProviderError
	if got := nilErr.Error(); got != "" {
		t.Errorf("nil error: expected empty, got %q", got)
	}

	pe := &ProviderError{Code: CodeRateLimited, Message: "too many requests"}
	msg := pe.Error()
	if !strings.Contains(msg, CodeRateLimited) || !strings.Contains(msg, "too many requests") {
		t.Errorf("unexpected error surface: %q", msg)
	}
}

func TestLimitedReadAll_HonoursCap(t *testing.T) {
	// Guard against a regression that would let a malicious upstream
	// exhaust memory. Feeding a reader that would produce more than
	// ReadAllLimit bytes must still return at most ReadAllLimit bytes
	// worth of data without error surface.
	r := &repeatReader{b: 'x', remaining: ReadAllLimit + 1024}
	out, err := LimitedReadAll(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != ReadAllLimit {
		t.Errorf("expected %d bytes, got %d", ReadAllLimit, len(out))
	}
}

// repeatReader yields a single byte repeatedly up to remaining, then EOFs.
type repeatReader struct {
	b         byte
	remaining int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, nil
	}
	n := len(p)
	if n > r.remaining {
		n = r.remaining
	}
	for i := range n {
		p[i] = r.b
	}
	r.remaining -= n
	return n, nil
}

// EmbeddingsInput discriminator coverage

// TestEmbeddingsInput_RoundTrip pins the JSON wire contract for all four
// legal OpenAI embedding input shapes. The discriminator is the single
// most failure-prone piece of the embedding canonical (mis-detecting a
// `[1,2,3]` token array as a string array silently corrupts cross-format
// routing), so the round-trip is exercised in both directions.
func TestEmbeddingsInput_RoundTrip(t *testing.T) {
	mkStr := func(s string) *string { return &s }
	tests := []struct {
		name      string
		input     EmbeddingsInput
		wantWire  string
		afterTrip EmbeddingsInput
	}{
		{
			name:      "bare string",
			input:     EmbeddingsInput{String: mkStr("hello")},
			wantWire:  `"hello"`,
			afterTrip: EmbeddingsInput{String: mkStr("hello")},
		},
		{
			name:      "array of strings",
			input:     EmbeddingsInput{Strings: []string{"a", "b", "c"}},
			wantWire:  `["a","b","c"]`,
			afterTrip: EmbeddingsInput{Strings: []string{"a", "b", "c"}},
		},
		{
			name:      "single token sequence flattens on the wire",
			input:     EmbeddingsInput{Tokens: [][]int{{1, 2, 3}}},
			wantWire:  `[1,2,3]`,
			afterTrip: EmbeddingsInput{Tokens: [][]int{{1, 2, 3}}},
		},
		{
			name:      "batch of token sequences",
			input:     EmbeddingsInput{Tokens: [][]int{{1, 2}, {3, 4, 5}}},
			wantWire:  `[[1,2],[3,4,5]]`,
			afterTrip: EmbeddingsInput{Tokens: [][]int{{1, 2}, {3, 4, 5}}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.input)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.wantWire {
				t.Fatalf("marshal: got %s, want %s", got, tc.wantWire)
			}
			var back EmbeddingsInput
			if err := json.Unmarshal(got, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !reflect.DeepEqual(back, tc.afterTrip) {
				t.Fatalf("round-trip: got %+v, want %+v", back, tc.afterTrip)
			}
		})
	}
}

// TestEmbeddingsInput_RoundTrip_SingleElementTokenBatch proves the wire-shape
// decision documented on MarshalJSON: a single-element Tokens batch encodes as
// a bare [1,2,3] array (not [[1,2,3]]), and decoding that bare array produces
// the same Tokens: [][]int{{1,2,3}} in-memory shape — the round-trip is lossless
// in the canonical→wire→canonical direction.
//
// The inverse is intentionally NOT lossless: a client that sends [[1,2,3]] (one
// batch entry) gets re-emitted as [1,2,3]. This is the chosen contract; callers
// that need to distinguish the two must inspect raw wire bytes before
// canonicalization.
func TestEmbeddingsInput_RoundTrip_SingleElementTokenBatch(t *testing.T) {
	original := EmbeddingsInput{Tokens: [][]int{{1, 2, 3}}}

	// Step 1: marshal → must produce bare [1,2,3], not [[1,2,3]].
	wire, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(wire) != `[1,2,3]` {
		t.Fatalf("single-element token batch must marshal as [1,2,3], got %s", wire)
	}

	// Step 2: unmarshal the wire bytes back → must recover the same shape.
	var decoded EmbeddingsInput
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Fatalf("canonical→wire→canonical round-trip failed: got %+v, want %+v", decoded, original)
	}

	// Step 3: re-marshal the decoded value → must be identical to the original wire.
	rewire, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(rewire) != string(wire) {
		t.Fatalf("re-marshal mismatch: got %s, want %s", rewire, wire)
	}
}

func TestEmbeddingsInput_MarshalZeroValue(t *testing.T) {
	var zero EmbeddingsInput
	out, err := json.Marshal(zero)
	if err != nil {
		t.Fatalf("marshal zero: %v", err)
	}
	if string(out) != "null" {
		t.Fatalf("zero value should marshal to null, got %s", out)
	}
}

func TestEmbeddingsInput_UnmarshalNullAndEmpty(t *testing.T) {
	t.Run("null literal", func(t *testing.T) {
		var v EmbeddingsInput
		if err := v.UnmarshalJSON([]byte("null")); err != nil {
			t.Fatalf("unmarshal null: %v", err)
		}
		if v.String != nil || v.Strings != nil || v.Tokens != nil {
			t.Fatalf("null should leave fields nil: %+v", v)
		}
	})
	t.Run("empty bytes", func(t *testing.T) {
		var v EmbeddingsInput
		if err := v.UnmarshalJSON(nil); err != nil {
			t.Fatalf("unmarshal empty: %v", err)
		}
	})
	t.Run("empty array", func(t *testing.T) {
		var v EmbeddingsInput
		if err := json.Unmarshal([]byte(`[]`), &v); err != nil {
			t.Fatalf("unmarshal empty array: %v", err)
		}
		if v.Strings == nil || len(v.Strings) != 0 {
			t.Fatalf("empty array should produce empty Strings, got %+v", v)
		}
	})
}

func TestEmbeddingsInput_UnmarshalErrors(t *testing.T) {
	cases := []struct {
		name  string
		wire  string
		check func(t *testing.T, err error)
	}{
		{
			name: "object instead of string or array",
			wire: `{"foo":"bar"}`,
			check: func(t *testing.T, err error) {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				if !strings.Contains(err.Error(), "expected string or array") {
					t.Fatalf("error should mention type mismatch, got: %v", err)
				}
			},
		},
		{
			name: "mixed types (string then int)",
			wire: `["hello", 42]`,
			check: func(t *testing.T, err error) {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				if !strings.Contains(err.Error(), "strings[1]") {
					t.Fatalf("error should pinpoint index 1: %v", err)
				}
			},
		},
		{
			name: "mixed types (int then string)",
			wire: `[1, "hello"]`,
			check: func(t *testing.T, err error) {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				if !strings.Contains(err.Error(), "tokens[1]") {
					t.Fatalf("error should pinpoint index 1: %v", err)
				}
			},
		},
		{
			name: "array of unsupported element (boolean)",
			wire: `[true, false]`,
			check: func(t *testing.T, err error) {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				if !strings.Contains(err.Error(), "unrecognised element type") {
					t.Fatalf("error should mention unrecognised element type: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var v EmbeddingsInput
			err := json.Unmarshal([]byte(tc.wire), &v)
			tc.check(t, err)
		})
	}
}

// TestEmbeddingsRequest_RoundTrip pins the request envelope shape that
// every embedding codec writes against. Drift here breaks every codec
// at the same time.
func TestEmbeddingsRequest_RoundTrip(t *testing.T) {
	dim := 1024
	enc := "base64"
	user := "abc-123"
	req := EmbeddingsRequest{
		Model:          "text-embedding-3-small",
		Input:          EmbeddingsInput{Strings: []string{"x", "y"}},
		Dimensions:     &dim,
		EncodingFormat: &enc,
		User:           &user,
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back EmbeddingsRequest
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Model != req.Model || *back.Dimensions != dim || *back.EncodingFormat != enc || *back.User != user {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
	if !reflect.DeepEqual(back.Input.Strings, []string{"x", "y"}) {
		t.Fatalf("input strings lost: %+v", back.Input)
	}
}

func TestEmbeddingsRequest_OmitsOptionalsWhenNil(t *testing.T) {
	req := EmbeddingsRequest{
		Model: "m",
		Input: EmbeddingsInput{String: func() *string { s := "hi"; return &s }()},
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	wire := string(body)
	for _, k := range []string{`"dimensions"`, `"encoding_format"`, `"user"`} {
		if strings.Contains(wire, k) {
			t.Fatalf("nil-pointer field %s should be omitted, got: %s", k, wire)
		}
	}
}

// TestEmbeddingsResponse_RoundTrip locks the canonical response envelope
// (Object/Data/Model/Usage) that decode paths emit. Base64 must never
// appear in the wire output (json:"-").
func TestEmbeddingsResponse_RoundTrip(t *testing.T) {
	resp := EmbeddingsResponse{
		Object: "list",
		Data: []EmbeddingDataItem{
			{Object: "embedding", Embedding: []float32{0.1, -0.2}, Index: 0},
			{Object: "embedding", Embedding: []float32{0.3, 0.4, 0.5}, Index: 1, Base64: "internal"},
		},
		Model: "text-embedding-3-small",
		Usage: EmbeddingsUsage{PromptTokens: 7, TotalTokens: 7},
	}
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	wire := string(body)
	if strings.Contains(wire, `"internal"`) {
		t.Fatalf("Base64 field must not appear on the wire (json:\"-\"), got: %s", wire)
	}
	var back EmbeddingsResponse
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatal(err)
	}
	if back.Object != "list" || back.Model != resp.Model || len(back.Data) != 2 {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
	if back.Usage.PromptTokens != 7 || back.Usage.TotalTokens != 7 {
		t.Fatalf("usage mismatch: %+v", back.Usage)
	}
	if !reflect.DeepEqual(back.Data[1].Embedding, []float32{0.3, 0.4, 0.5}) {
		t.Fatalf("data[1] vector lost: %+v", back.Data[1])
	}
}

// TestArtifactKindAndJobStatus_Constants is a guard so a typo in any of
// the four artifact kinds or five job-status enums fails at build time.
// Future artifact-type features will key off these strings; locking them now prevents silent
// drift later.
func TestArtifactKindAndJobStatus_Constants(t *testing.T) {
	if ArtifactKindImage != "image" || ArtifactKindAudio != "audio" || ArtifactKindVideo != "video" || ArtifactKindJob != "job" {
		t.Fatalf("artifact kind constants drifted")
	}
	if JobStatusQueued != "queued" || JobStatusRunning != "running" || JobStatusSucceeded != "succeeded" ||
		JobStatusFailed != "failed" || JobStatusCanceled != "canceled" {
		t.Fatalf("job status constants drifted")
	}
	// Sanity: ArtifactRef and JobRef can be zero-valued (no required init).
	_ = ArtifactRef{}
	_ = JobRef{}
}
