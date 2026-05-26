package vertex

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"golang.org/x/oauth2"
)

// genServiceAccountJSON synthesizes a credentials.json blob acceptable to
// google.JWTConfigFromJSON. The PEM-encoded private key must parse via
// x509; the token_uri is overridable so each test can point the JWT flow
// at its own httptest server.
func genServiceAccountJSON(t *testing.T, tokenURI, email string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	// The x/oauth2 jwt package accepts PKCS1 ("RSA PRIVATE KEY") and PKCS8
	// ("PRIVATE KEY"); we use PKCS8 to match the real GCP credentials shape.
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("pkcs8: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: pkcs8,
	})
	cred := map[string]any{
		"type":           "service_account",
		"client_email":   email,
		"private_key":    string(pemBytes),
		"private_key_id": "kid-1",
		"token_uri":      tokenURI,
		"project_id":     "p-test",
	}
	b, err := json.Marshal(cred)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// newTokenServer spins up a fake GCP token endpoint that returns the
// supplied access token + ttl.
func newTokenServer(t *testing.T, accessToken string, ttlSeconds int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Errorf("grant_type=%q", got)
		}
		if r.Form.Get("assertion") == "" {
			t.Errorf("missing assertion")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w,
			`{"access_token":%q,"token_type":"Bearer","expires_in":%d}`,
			accessToken, ttlSeconds)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestNewSpec_NilLogReplaced covers spec.go:16-18 — nil logger gets
// replaced by slog.Default, and the spec is fully wired.
func TestNewSpec_NilLogReplaced(t *testing.T) {
	s := NewSpec(nil)
	if s.Format != provcore.FormatVertex {
		t.Errorf("format=%q want vertex", s.Format)
	}
	if s.Transport == nil || s.SchemaCodec == nil || s.StreamDecoder == nil || s.ErrorNormalizer == nil {
		t.Errorf("AdapterSpec incomplete: %+v", s)
	}
}

// TestNewSpec_CustomLogKept covers the non-nil log branch.
func TestNewSpec_CustomLogKept(t *testing.T) {
	s := NewSpec(slog.Default())
	if s.Format != provcore.FormatVertex {
		t.Errorf("format=%q", s.Format)
	}
}

// TestNewTransport_NilLogReplaced covers transport.go:42-44.
func TestNewTransport_NilLogReplaced(t *testing.T) {
	tr := NewTransport(nil)
	if tr == nil {
		t.Fatal("NewTransport returned nil")
	}
	if tr.log == nil {
		t.Errorf("nil-log path did not fall back to slog.Default")
	}
}

// TestBuildURL_DefaultsAndAllPaths covers transport.go:54-96. Pin every
// branch: default location ("us-central1"), default publisher ("google"),
// derived BaseURL when empty, models endpoint, unsupported endpoint, and
// each missing-prerequisite error.
func TestBuildURL_DefaultsAndAllPaths(t *testing.T) {
	tr := NewTransport(slog.Default())

	t.Run("default_location_default_publisher_derived_base", func(t *testing.T) {
		// BaseURL empty + projectId present → derive
		// https://<location>-aiplatform.googleapis.com.
		// Publisher empty → "google".
		got, err := tr.BuildURL(
			provcore.CallTarget{
				ProviderModelID: "gemini-1.5-pro",
				Extras:          map[string]string{"gcp.projectId": "p"},
			},
			typology.WireShapeVertexGenerateContent,
			false,
		)
		if err != nil {
			t.Fatal(err)
		}
		want := "https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/us-central1/publishers/google/models/gemini-1.5-pro:generateContent"
		if got != want {
			t.Errorf("got=%q\nwant=%q", got, want)
		}
	})

	t.Run("custom_publisher", func(t *testing.T) {
		got, err := tr.BuildURL(
			provcore.CallTarget{
				ProviderModelID: "claude-3-sonnet@20240620",
				Extras: map[string]string{
					"gcp.projectId": "p",
					"gcp.publisher": "anthropic",
					"gcp.location":  "us-east5",
				},
			},
			typology.WireShapeVertexGenerateContent,
			false,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "/publishers/anthropic/models/claude-3-sonnet@20240620:generateContent") {
			t.Errorf("path: %s", got)
		}
		if !strings.HasPrefix(got, "https://us-east5-aiplatform.googleapis.com/") {
			t.Errorf("derived base: %s", got)
		}
	})

	t.Run("stream_sse_query", func(t *testing.T) {
		got, err := tr.BuildURL(
			provcore.CallTarget{
				BaseURL:         "https://override.example.com",
				ProviderModelID: "gemini-1.5-pro",
				Extras:          map[string]string{"gcp.projectId": "p"},
			},
			typology.WireShapeVertexGenerateContent,
			true,
		)
		if err != nil {
			t.Fatal(err)
		}
		// Caller-provided BaseURL is honored verbatim (no derivation),
		// trailing slash stripped, stream action + alt=sse appended.
		if !strings.HasPrefix(got, "https://override.example.com/v1/projects/p/locations/us-central1/") {
			t.Errorf("base override not honored: %s", got)
		}
		if !strings.HasSuffix(got, ":streamGenerateContent?alt=sse") {
			t.Errorf("stream suffix missing: %s", got)
		}
	})

	t.Run("base_trailing_slash_stripped", func(t *testing.T) {
		got, err := tr.BuildURL(
			provcore.CallTarget{
				BaseURL:         "https://x/",
				ProviderModelID: "m",
				Extras:          map[string]string{"gcp.projectId": "p"},
			},
			typology.WireShapeVertexGenerateContent,
			false,
		)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got, "//v1") {
			t.Errorf("double slash in url: %s", got)
		}
	})

	t.Run("models_endpoint", func(t *testing.T) {
		got, err := tr.BuildURL(
			provcore.CallTarget{
				BaseURL:         "https://x",
				ProviderModelID: "ignored-for-listing",
				Extras: map[string]string{
					"gcp.projectId": "p",
					"gcp.location":  "europe-west4",
				},
			},
			typology.WireShapeNone,
			false,
		)
		if err != nil {
			t.Fatal(err)
		}
		want := "https://x/v1/projects/p/locations/europe-west4/publishers/google/models"
		if got != want {
			t.Errorf("got=%q want=%q", got, want)
		}
	})

	t.Run("missing_base_and_missing_project", func(t *testing.T) {
		// Hits "missing BaseURL and gcp.projectId" — fires before the
		// model-id check.
		_, err := tr.BuildURL(
			provcore.CallTarget{ProviderModelID: "m"},
			typology.WireShapeVertexGenerateContent,
			false,
		)
		if err == nil || !strings.Contains(err.Error(), "missing BaseURL and gcp.projectId") {
			t.Errorf("err=%v", err)
		}
	})

	t.Run("missing_model_id", func(t *testing.T) {
		_, err := tr.BuildURL(
			provcore.CallTarget{
				BaseURL: "https://x",
				Extras:  map[string]string{"gcp.projectId": "p"},
			},
			typology.WireShapeVertexGenerateContent,
			false,
		)
		if err == nil || !strings.Contains(err.Error(), "missing ProviderModelID") {
			t.Errorf("err=%v", err)
		}
	})

	t.Run("missing_project_when_base_supplied", func(t *testing.T) {
		// BaseURL bypasses the first guard, so the project guard at L75-77
		// fires instead.
		_, err := tr.BuildURL(
			provcore.CallTarget{
				BaseURL:         "https://x",
				ProviderModelID: "m",
			},
			typology.WireShapeVertexGenerateContent,
			false,
		)
		if err == nil || !strings.Contains(err.Error(), "missing gcp.projectId") {
			t.Errorf("err=%v", err)
		}
	})

	t.Run("unsupported_endpoint", func(t *testing.T) {
		_, err := tr.BuildURL(
			provcore.CallTarget{
				BaseURL:         "https://x",
				ProviderModelID: "m",
				Extras:          map[string]string{"gcp.projectId": "p"},
			},
			typology.WireShape("bogus"),
			false,
		)
		if err == nil || !strings.Contains(err.Error(), "unsupported endpoint") {
			t.Errorf("err=%v", err)
		}
	})
}

// TestApplyAuth_AllCredentialSources covers transport.go:100-107 paired
// with token() at 153-191.
func TestApplyAuth_AllCredentialSources(t *testing.T) {
	tr := NewTransport(slog.Default())

	t.Run("bearer_token_passthrough", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
		err := tr.ApplyAuth(req, provcore.CallTarget{
			Extras: map[string]string{"gcp.bearerToken": "ya29.opaque"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer ya29.opaque" {
			t.Errorf("Authorization=%q", got)
		}
	})

	t.Run("api_key_as_bearer_fallback", func(t *testing.T) {
		// No saJSON, no bearer → APIKey is used.
		req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
		err := tr.ApplyAuth(req, provcore.CallTarget{APIKey: "raw-key-123"})
		if err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer raw-key-123" {
			t.Errorf("Authorization=%q", got)
		}
	})

	t.Run("nothing_supplied_errors", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
		err := tr.ApplyAuth(req, provcore.CallTarget{})
		if err == nil || !strings.Contains(err.Error(), "missing gcp.serviceAccountJSON") {
			t.Errorf("err=%v", err)
		}
	})
}

// TestApplyAuth_ServiceAccountJSON_MintsAndCaches covers transport.go:165-190
// — full OAuth2 JWT flow against a httptest token endpoint, plus the
// warm-cache short-circuit at L171-175.
func TestApplyAuth_ServiceAccountJSON_MintsAndCaches(t *testing.T) {
	calls := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// 1h expiry so the cache stays warm well past the 60s guard.
		_, _ = w.Write([]byte(`{"access_token":"minted-1","token_type":"Bearer","expires_in":3600}`))
	}))
	defer srv.Close()

	saJSON := genServiceAccountJSON(t, srv.URL, "sa@p.iam.gserviceaccount.com")
	tr := NewTransport(slog.Default())
	target := provcore.CallTarget{
		Extras: map[string]string{
			"gcp.serviceAccountJSON": saJSON,
			"gcp.projectId":          "p",
		},
	}

	// First call mints, stamps Bearer header.
	req1, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	if err := tr.ApplyAuth(req1, target); err != nil {
		t.Fatalf("first ApplyAuth: %v", err)
	}
	if req1.Header.Get("Authorization") != "Bearer minted-1" {
		t.Errorf("first Authorization=%q", req1.Header.Get("Authorization"))
	}

	// Second call must hit the cache — token endpoint stays at 1 hit.
	req2, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	if err := tr.ApplyAuth(req2, target); err != nil {
		t.Fatalf("second ApplyAuth: %v", err)
	}
	if req2.Header.Get("Authorization") != "Bearer minted-1" {
		t.Errorf("cached Authorization=%q", req2.Header.Get("Authorization"))
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("token endpoint hit %d times, want 1 (cache miss)", calls)
	}
}

// TestApplyAuth_ServiceAccountJSON_ExpiredCacheRemints covers the
// "ok && time.Until(entry.expires) > 60s" branch at L174 by seeding a
// near-expired entry directly into the cache.
func TestApplyAuth_ServiceAccountJSON_ExpiredCacheRemints(t *testing.T) {
	srv := newTokenServer(t, "fresh-token", 3600)
	saJSON := genServiceAccountJSON(t, srv.URL, "sa@p.iam.gserviceaccount.com")
	tr := NewTransport(slog.Default())

	// Pre-seed cache with an entry whose Expiry is well under the 60s guard.
	tr.cacheMu.Lock()
	tr.cache["sa@p.iam.gserviceaccount.com|p"] = &tokenCacheEntry{
		token:   &oauth2.Token{AccessToken: "stale-token", Expiry: time.Now().Add(5 * time.Second)},
		expires: time.Now().Add(5 * time.Second),
	}
	tr.cacheMu.Unlock()

	req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	err := tr.ApplyAuth(req, provcore.CallTarget{
		Extras: map[string]string{
			"gcp.serviceAccountJSON": saJSON,
			"gcp.projectId":          "p",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Must remint (stale → fresh).
	if got := req.Header.Get("Authorization"); got != "Bearer fresh-token" {
		t.Errorf("Authorization=%q want Bearer fresh-token (cache should have been bypassed)", got)
	}
}

// TestApplyAuth_BadServiceAccountJSON covers serviceAccountEmail's
// json.Unmarshal failure (transport.go:199-200) and the missing-email
// branch (L202-204).
func TestApplyAuth_BadServiceAccountJSON(t *testing.T) {
	t.Run("malformed_json", func(t *testing.T) {
		tr := NewTransport(slog.Default())
		req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
		err := tr.ApplyAuth(req, provcore.CallTarget{
			Extras: map[string]string{"gcp.serviceAccountJSON": "{not json"},
		})
		if err == nil || !strings.Contains(err.Error(), "parse service-account JSON") {
			t.Errorf("err=%v", err)
		}
	})

	t.Run("missing_client_email", func(t *testing.T) {
		tr := NewTransport(slog.Default())
		req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
		err := tr.ApplyAuth(req, provcore.CallTarget{
			Extras: map[string]string{"gcp.serviceAccountJSON": `{"client_email":""}`},
		})
		if err == nil || !strings.Contains(err.Error(), "missing client_email") {
			t.Errorf("err=%v", err)
		}
	})

	t.Run("json_valid_but_wrong_type_field", func(t *testing.T) {
		// JSON parses, email present (so serviceAccountEmail succeeds), but
		// google.JWTConfigFromJSON rejects because type != "service_account".
		// Covers transport.go:178-181 — "vertex: parse service-account JSON".
		tr := NewTransport(slog.Default())
		req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
		err := tr.ApplyAuth(req, provcore.CallTarget{
			Extras: map[string]string{
				"gcp.serviceAccountJSON": `{"client_email":"sa@p.iam.gserviceaccount.com","type":"authorized_user"}`,
			},
		})
		if err == nil {
			t.Fatal("expected error from wrong type field")
		}
		if !strings.Contains(err.Error(), "parse service-account JSON") {
			t.Errorf("err=%v want 'parse service-account JSON' wrapper", err)
		}
	})

	t.Run("json_valid_but_no_private_key", func(t *testing.T) {
		// JSON parses, email present, but the JWT cfg load fails because
		// PrivateKey is empty. Covers transport.go:178-181.
		tr := NewTransport(slog.Default())
		req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
		// We must omit private_key entirely — JWTConfigFromJSON tolerates
		// the missing key but TokenSource().Token() will then fail on the
		// PEM parse. We expect *some* error; the wrapper "mint oauth2
		// token" message must appear.
		err := tr.ApplyAuth(req, provcore.CallTarget{
			Extras: map[string]string{
				"gcp.serviceAccountJSON": `{
					"type":"service_account",
					"client_email":"sa@p.iam.gserviceaccount.com",
					"private_key":"-----BEGIN PRIVATE KEY-----\nBAD\n-----END PRIVATE KEY-----\n",
					"token_uri":"http://127.0.0.1:1"
				}`,
			},
		})
		if err == nil {
			t.Fatal("expected error from bad private key")
		}
		if !strings.Contains(err.Error(), "mint oauth2 token") {
			t.Errorf("err=%v want 'mint oauth2 token' wrapper", err)
		}
	})
}

// TestProbe_AllBranches covers transport.go:115-148.
func TestProbe_AllBranches(t *testing.T) {
	t.Run("missing_project_id", func(t *testing.T) {
		tr := NewTransport(slog.Default())
		res, err := tr.Probe(context.Background(), provcore.CallTarget{})
		if err != nil {
			t.Fatal(err)
		}
		if res.OK || !strings.Contains(res.Detail, "missing gcp.projectId") {
			t.Errorf("res=%+v", res)
		}
	})

	t.Run("token_mint_fails", func(t *testing.T) {
		// No credentials supplied → token() returns error → Probe returns
		// OK=false with err wired through.
		tr := NewTransport(slog.Default())
		res, err := tr.Probe(context.Background(), provcore.CallTarget{
			Extras: map[string]string{"gcp.projectId": "p"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.OK || res.Err == nil {
			t.Errorf("expected token-mint failure, got %+v", res)
		}
		if !strings.Contains(res.Detail, "missing gcp.serviceAccountJSON") {
			t.Errorf("Detail=%q", res.Detail)
		}
	})

	// The happy-path / non-2xx / transport-error branches at L132-147 all
	// hit https://<location>-aiplatform.googleapis.com which we cannot
	// re-route via BaseURL (Probe doesn't read BaseURL). We exercise
	// those branches by pointing a custom HTTP transport at our test
	// server. Specifically we substitute tr.probe with an http.Client
	// whose Transport rewrites the request URL to our test server.

	rewritingClient := func(target *httptest.Server) *http.Client {
		return &http.Client{Transport: rewriteRoundTripper{target: target}}
	}

	t.Run("probe_ok_2xx", func(t *testing.T) {
		gotAuth := ""
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		tr := NewTransport(slog.Default())
		tr.probe = rewritingClient(srv)
		res, err := tr.Probe(context.Background(), provcore.CallTarget{
			Extras: map[string]string{
				"gcp.projectId":   "p",
				"gcp.bearerToken": "ya29.test",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !res.OK {
			t.Errorf("res=%+v want OK", res)
		}
		if res.Detail != "ok" {
			t.Errorf("Detail=%q", res.Detail)
		}
		if gotAuth != "Bearer ya29.test" {
			t.Errorf("Authorization=%q", gotAuth)
		}
	})

	t.Run("probe_non_2xx", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		tr := NewTransport(slog.Default())
		tr.probe = rewritingClient(srv)
		res, err := tr.Probe(context.Background(), provcore.CallTarget{
			Extras: map[string]string{
				"gcp.projectId":   "p",
				"gcp.bearerToken": "x",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.OK {
			t.Errorf("res should not be OK: %+v", res)
		}
		if !strings.Contains(res.Detail, "HTTP 403") {
			t.Errorf("Detail=%q want contain HTTP 403", res.Detail)
		}
	})

	t.Run("probe_transport_error", func(t *testing.T) {
		// Closed server → dial fails. We deliberately don't substitute
		// the transport so the real DNS path runs against an unroutable
		// host derived from the (default) location.
		tr := NewTransport(slog.Default())
		// Replace probe client with one whose RoundTrip always errors.
		tr.probe = &http.Client{Transport: errRoundTripper{}}
		res, err := tr.Probe(context.Background(), provcore.CallTarget{
			Extras: map[string]string{
				"gcp.projectId":   "p",
				"gcp.bearerToken": "x",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.OK || res.Err == nil {
			t.Errorf("expected transport error: %+v", res)
		}
	})

	t.Run("custom_location_passed_through", func(t *testing.T) {
		// Verify the location extra propagates into the probe URL host
		// AND path (covers the urlStr formatting at L132).
		var gotURL string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotURL = r.URL.String()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		tr := NewTransport(slog.Default())
		// Wrap the probe RT so we can both inspect the original Host
		// and successfully serve a response.
		var capturedHost string
		tr.probe = &http.Client{Transport: hostCapturingRT{target: srv, captured: &capturedHost}}
		res, _ := tr.Probe(context.Background(), provcore.CallTarget{
			Extras: map[string]string{
				"gcp.projectId":   "p",
				"gcp.location":    "asia-northeast1",
				"gcp.bearerToken": "x",
			},
		})
		if !res.OK {
			t.Errorf("res=%+v", res)
		}
		if !strings.HasPrefix(capturedHost, "asia-northeast1-aiplatform.googleapis.com") {
			t.Errorf("host=%q want asia-northeast1-aiplatform.googleapis.com…", capturedHost)
		}
		if !strings.Contains(gotURL, "/v1/projects/p/locations/asia-northeast1/publishers/google/models") {
			t.Errorf("path: %s", gotURL)
		}
		if !strings.Contains(gotURL, "pageSize=1") {
			t.Errorf("pageSize: %s", gotURL)
		}
	})
}

// rewriteRoundTripper redirects every request to a single httptest
// server while preserving headers — used to exercise Probe's HTTPS URL
// composition without external DNS.
type rewriteRoundTripper struct{ target *httptest.Server }

func (r rewriteRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Re-issue the request against the test server.
	newURL := r.target.URL + req.URL.Path
	if req.URL.RawQuery != "" {
		newURL += "?" + req.URL.RawQuery
	}
	req2, err := http.NewRequestWithContext(req.Context(), req.Method, newURL, req.Body)
	if err != nil {
		return nil, err
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			req2.Header.Add(k, v)
		}
	}
	return r.target.Client().Do(req2)
}

// hostCapturingRT records the original request Host so the test can
// assert the URL composition, then dispatches to the local test server.
type hostCapturingRT struct {
	target   *httptest.Server
	captured *string
}

func (h hostCapturingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	*h.captured = req.URL.Host
	return rewriteRoundTripper{target: h.target}.RoundTrip(req)
}

// errRoundTripper always errors — used to exercise Probe's network
// failure branch without depending on external DNS.
type errRoundTripper struct{}

func (errRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("simulated transport error")
}

// TestDo_DelegatesWithContext covers transport.go:110-112. The Do
// contract is "issue against the shared client with the request's
// context"; verify the upstream is hit and a cancelled context aborts.
func TestDo_DelegatesWithContext(t *testing.T) {
	t.Run("happy_path", func(t *testing.T) {
		called := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		tr := NewTransport(slog.Default())
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		resp, err := tr.Do(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		if !called {
			t.Errorf("upstream not called")
		}
	})

	t.Run("cancelled_context_aborts", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(200 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		tr := NewTransport(slog.Default())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		resp, err := tr.Do(ctx, req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err == nil {
			t.Fatal("expected ctx error")
		}
	})
}
