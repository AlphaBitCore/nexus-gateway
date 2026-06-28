// Tests for Transport.ListModels — the OpenAI /v1/models list fetcher
// used by the provider-discover admin path.
//
// Coverage targets:
//   - happy path: server returns well-formed list → exact model IDs returned
//   - non-2xx: 401 response → error containing HTTP status code
//   - malformed body: server returns non-JSON → decode error
//   - empty BaseURL: CallTarget with no BaseURL → fast error, no HTTP call
package openai_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
)

// TestListModels_ParsesOpenAIList covers the happy path: a conforming
// OpenAI list envelope is parsed and the model IDs are returned in order.
func TestListModels_ParsesOpenAIList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %q; want /v1/models", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %q; want GET", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("Authorization header %q; want 'Bearer k'", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"mock-gpt-4o-mini","object":"model"},{"id":"mock-text-embedding-3-small","object":"model"}]}`))
	}))
	defer srv.Close()

	got, err := openai.NewTransport(nil).ListModels(context.Background(), provcore.CallTarget{
		BaseURL: srv.URL,
		APIKey:  "k",
	})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []string{"mock-gpt-4o-mini", "mock-text-embedding-3-small"}
	if len(got) != len(want) {
		t.Fatalf("len(ids)=%d want %d; ids=%v", len(got), len(want), got)
	}
	for i, id := range want {
		if got[i] != id {
			t.Errorf("ids[%d]=%q want %q", i, got[i], id)
		}
	}
}

// TestListModels_Non2xxIsError covers the non-2xx branch: a 401 response
// must produce a non-nil error that identifies the HTTP status.
func TestListModels_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := openai.NewTransport(nil).ListModels(context.Background(), provcore.CallTarget{
		BaseURL: srv.URL,
		APIKey:  "bad-key",
	})
	if err == nil {
		t.Fatal("expected error on HTTP 401; got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error %q must mention status 401", err.Error())
	}
}

// TestListModels_MalformedBodyIsError covers the JSON decode failure branch:
// the server replies 200 with non-JSON body; ListModels must return an error.
func TestListModels_MalformedBodyIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	_, err := openai.NewTransport(nil).ListModels(context.Background(), provcore.CallTarget{
		BaseURL: srv.URL,
		APIKey:  "k",
	})
	if err == nil {
		t.Fatal("expected error on malformed JSON body; got nil")
	}
}

// TestListModels_EmptyBaseURL covers the empty BaseURL fast-path: no HTTP
// request must be issued; an error is returned immediately.
func TestListModels_EmptyBaseURL(t *testing.T) {
	_, err := openai.NewTransport(nil).ListModels(context.Background(), provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error on empty BaseURL; got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "baseurl") {
		t.Errorf("error %q must mention BaseURL", err.Error())
	}
}
