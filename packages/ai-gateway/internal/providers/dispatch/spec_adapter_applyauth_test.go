package dispatch

import (
	"errors"
	"net/http"
	"testing"

	"log/slog"
)

// authAsserter mirrors the optional interface the STT streaming-proxy handler
// type-asserts on the concrete adapter (kept off the Adapter interface so no
// test double has to grow it).
type authAsserter interface {
	ApplyAuth(*http.Request, CallTarget) error
}

func TestSpecAdapter_ApplyAuth_DelegatesToTransport(t *testing.T) {
	tr := &fakeTransport{}
	adapter := NewSpecAdapter(specFrom(tr, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())

	aa, ok := adapter.(authAsserter)
	if !ok {
		t.Fatalf("specAdapter does not expose ApplyAuth via the optional interface")
	}

	req, _ := http.NewRequest(http.MethodPost, "https://upstream.test/v1/audio/transcriptions", nil)
	if err := aa.ApplyAuth(req, CallTarget{APIKey: "sk-secret"}); err != nil {
		t.Fatalf("ApplyAuth error: %v", err)
	}
	// The default fakeTransport.ApplyAuth writes Bearer <APIKey> — proving the
	// method delegates to the spec's Transport.
	if got := req.Header.Get("Authorization"); got != "Bearer sk-secret" {
		t.Errorf("Authorization = %q, want Bearer sk-secret", got)
	}
}

func TestSpecAdapter_ApplyAuth_PropagatesTransportError(t *testing.T) {
	wantErr := errors.New("missing api key")
	tr := &fakeTransport{applyAuth: func(*http.Request, CallTarget) error { return wantErr }}
	adapter := NewSpecAdapter(specFrom(tr, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())

	aa := adapter.(authAsserter)
	req, _ := http.NewRequest(http.MethodPost, "https://upstream.test", nil)
	if err := aa.ApplyAuth(req, CallTarget{}); !errors.Is(err, wantErr) {
		t.Errorf("ApplyAuth error = %v, want %v", err, wantErr)
	}
}
