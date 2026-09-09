package tlsbump

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// nexusStripRoundTripper records the request as it went on the wire.
type nexusStripRoundTripper struct{ got http.Header }

func (c *nexusStripRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	c.got = r.Header.Clone()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    r,
	}, nil
}

// TestForwardRequest_StripsTheNexusNamespace pins the non-disclosure rule for
// the intercepting services.
//
// The compliance proxy and the agent sit in front of traffic the caller
// addressed to a provider, not to Nexus. Anything we add to that request tells
// a third party that Nexus is in the path, and the correlation id we mint is a
// value of ours that has no business travelling to OpenAI or Anthropic. The AI
// Gateway has denied this prefix toward providers for a long time; this is the
// same rule on the interception path.
//
// The client-supplied case matters as much as the minted one: a client that
// speaks the Nexus vocabulary is addressing Nexus, so those headers stop here
// too rather than continuing to a provider that never asked for them.
func TestForwardRequest_StripsTheNexusNamespace(t *testing.T) {
	tests := []struct {
		name   string
		header string
		value  string
	}{
		{"minted correlation id", "X-Nexus-Request-Id", "9f2c1e77-3b4a-4d5e-8f6a-7b8c9d0e1f20"},
		{"client-sent correlation id", "X-Nexus-Request-Id", "caller-supplied"},
		{"attribution tag", "X-Nexus-End-User-Id", "acct_8817"},
		{"session tag", "X-Nexus-Session-Id", "thread_20a4"},
		{"client tags", "X-Nexus-Client-Tags", "tenant=acme"},
		{"lower-case spelling", "x-nexus-request-id", "lowercase-sender"},
		{"a name that does not exist yet", "X-Nexus-Something-Future", "v"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := &nexusStripRoundTripper{}
			u := &UpstreamTransport{transport: rt}

			req := httptest.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", nil)
			req.Header.Set(tc.header, tc.value)
			// A header the provider genuinely needs, to prove the strip is
			// scoped to our namespace and is not deleting the request.
			req.Header.Set("Content-Type", "application/json")

			resp, err := u.ForwardRequest(context.Background(), req)
			if err != nil {
				t.Fatalf("ForwardRequest: %v", err)
			}
			defer resp.Body.Close()

			for name := range rt.got {
				if strings.HasPrefix(strings.ToLower(name), "x-nexus-") {
					t.Errorf("%s = %q reached the upstream — the X-Nexus-* namespace must not leave the interception point",
						name, rt.got.Get(name))
				}
			}
			if rt.got.Get("Content-Type") != "application/json" {
				t.Errorf("Content-Type = %q, want it forwarded untouched — the strip must be scoped to our namespace",
					rt.got.Get("Content-Type"))
			}
		})
	}
}

// TestForwardRequest_LeavesTheCallersOwnRequestIDAlone guards the boundary from
// the other side. `X-Request-Id` carries no Nexus prefix because it is the
// caller's own header under the industry-conventional name; providers read and
// echo it, and stripping it would break a correlation the caller set up with
// their provider directly.
func TestForwardRequest_LeavesTheCallersOwnRequestIDAlone(t *testing.T) {
	rt := &nexusStripRoundTripper{}
	u := &UpstreamTransport{transport: rt}

	req := httptest.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", nil)
	req.Header.Set("X-Request-Id", "caller-own-id")

	resp, err := u.ForwardRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("ForwardRequest: %v", err)
	}
	defer resp.Body.Close()

	if got := rt.got.Get("X-Request-Id"); got != "caller-own-id" {
		t.Errorf("X-Request-Id = %q, want it forwarded: it is the caller's header, not ours", got)
	}
}

// TestForwardRequest_InjectorRunsAfterTheStrip pins an ordering the strip could
// silently break. The agent stamps X-Nexus-Attestation through the per-request
// injector, and that header is in the namespace the strip removes — so the two
// only coexist because the injector runs later. Reorder them and attestation
// disappears from every request, the compliance proxy falls back to treating
// the agent as unattested, and nothing fails loudly enough to notice.
func TestForwardRequest_InjectorRunsAfterTheStrip(t *testing.T) {
	rt := &nexusStripRoundTripper{}
	u := &UpstreamTransport{
		transport: rt,
		requestInjector: func(r *http.Request) error {
			r.Header.Set("X-Nexus-Attestation", "signed-blob")
			return nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", nil)
	// A header the strip must remove, to prove both halves run.
	req.Header.Set("X-Nexus-Request-Id", "minted-here")

	resp, err := u.ForwardRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("ForwardRequest: %v", err)
	}
	defer resp.Body.Close()

	if got := rt.got.Get("X-Nexus-Attestation"); got != "signed-blob" {
		t.Errorf("X-Nexus-Attestation = %q, want it to survive — the injector must run after the strip, or the agent silently stops attesting", got)
	}
	if got := rt.got.Get("X-Nexus-Request-Id"); got != "" {
		t.Errorf("X-Nexus-Request-Id = %q, want it stripped — this half proves the strip ran at all", got)
	}
}
