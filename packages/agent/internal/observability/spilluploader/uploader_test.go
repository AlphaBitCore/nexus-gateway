package spilluploader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHub implements HubClient backed by an httptest.Server.
type fakeHub struct {
	srv *httptest.Server
}

func (f *fakeHub) BaseURL() string          { return f.srv.URL }
func (f *fakeHub) HTTPClient() *http.Client { return f.srv.Client() }

func TestUpload_NilUploaderFallsBackInline(t *testing.T) {
	var u *Uploader
	_, err := u.Upload(context.Background(), "evt", "req", "application/json", []byte("body"))
	if !errors.Is(err, ErrFallbackInline) {
		t.Errorf("nil uploader: got %v, want ErrFallbackInline", err)
	}
}

func TestUpload_NilHubFallsBackInline(t *testing.T) {
	u := New(nil)
	_, err := u.Upload(context.Background(), "evt", "req", "application/json", []byte("body"))
	if !errors.Is(err, ErrFallbackInline) {
		t.Errorf("nil hub: got %v, want ErrFallbackInline", err)
	}
}

func TestUpload_MissingEventIDFallsBack(t *testing.T) {
	u := New(&fakeHub{srv: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("hub must not be contacted when eventID missing")
	}))})
	_, err := u.Upload(context.Background(), "", "req", "application/json", []byte("body"))
	if !errors.Is(err, ErrFallbackInline) {
		t.Errorf("missing eventID: %v", err)
	}
}

func TestUpload_MissingDirectionFallsBack(t *testing.T) {
	u := New(&fakeHub{srv: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("hub must not be contacted when direction missing")
	}))})
	_, err := u.Upload(context.Background(), "evt", "", "application/json", []byte("body"))
	if !errors.Is(err, ErrFallbackInline) {
		t.Errorf("missing direction: %v", err)
	}
}

func TestUpload_EmptyBodyFallsBack(t *testing.T) {
	u := New(&fakeHub{srv: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("hub must not be contacted for empty body")
	}))})
	_, err := u.Upload(context.Background(), "evt", "req", "application/json", nil)
	if !errors.Is(err, ErrFallbackInline) {
		t.Errorf("empty body: %v", err)
	}
}

func TestUpload_SuccessfulRoundTrip(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	wantHash := sha256Hex(body)

	var mintCalled, putCalled atomic.Int32
	var mintBody mintRequest
	var putBody []byte
	var putContentType string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/internal/things/spill-uploads", func(w http.ResponseWriter, r *http.Request) {
		mintCalled.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("mint: method = %s", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("mint: content-type = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&mintBody); err != nil {
			t.Errorf("mint: decode: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mintResponse{
			UploadURL: "http://" + r.Host + "/upload/abc",
			Key:       "spill/abc",
			Backend:   "s3",
			ExpiresAt: time.Now().Add(5 * time.Minute),
		})
	})
	mux.HandleFunc("/upload/abc", func(w http.ResponseWriter, r *http.Request) {
		putCalled.Add(1)
		if r.Method != http.MethodPut {
			t.Errorf("put: method = %s", r.Method)
		}
		putContentType = r.Header.Get("Content-Type")
		putBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Fix uploadURL to use this server's URL (mux returned r.URL.Scheme but
	// httptest sets http and r.Host = host:port, which is what we want).
	mux.HandleFunc("/upload-final/", func(w http.ResponseWriter, r *http.Request) {})

	u := New(&fakeHub{srv: srv})
	ref, err := u.Upload(context.Background(), "evt-1", "request", "application/json", body)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	if mintCalled.Load() != 1 {
		t.Errorf("mint called %d times, want 1", mintCalled.Load())
	}
	if putCalled.Load() != 1 {
		t.Errorf("put called %d times, want 1", putCalled.Load())
	}

	// mint request payload assertions.
	if mintBody.EventID != "evt-1" {
		t.Errorf("mint eventId: %q", mintBody.EventID)
	}
	if mintBody.Direction != "request" {
		t.Errorf("mint direction: %q", mintBody.Direction)
	}
	if mintBody.SizeBytes != int64(len(body)) {
		t.Errorf("mint sizeBytes: %d, want %d", mintBody.SizeBytes, len(body))
	}
	if mintBody.SHA256 != wantHash {
		t.Errorf("mint sha256: got %q, want %q", mintBody.SHA256, wantHash)
	}
	if mintBody.ContentType != "application/json" {
		t.Errorf("mint contentType: %q", mintBody.ContentType)
	}

	// PUT body must be byte-identical to input.
	if string(putBody) != string(body) {
		t.Errorf("put body roundtrip mismatch:\n got=%q\nwant=%q", putBody, body)
	}
	if putContentType != "application/json" {
		t.Errorf("put contentType: %q", putContentType)
	}

	// Returned SpillRef must carry all derived fields.
	if ref.Backend != "s3" {
		t.Errorf("ref.Backend: %q", ref.Backend)
	}
	if ref.Key != "spill/abc" {
		t.Errorf("ref.Key: %q", ref.Key)
	}
	if ref.Size != int64(len(body)) {
		t.Errorf("ref.Size: %d", ref.Size)
	}
	if ref.SHA256 != wantHash {
		t.Errorf("ref.SHA256: %q, want %q", ref.SHA256, wantHash)
	}
	if ref.ContentType != "application/json" {
		t.Errorf("ref.ContentType: %q", ref.ContentType)
	}
}

func TestUpload_MintNon200FallsBackInlineWithErrorBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/internal/things/spill-uploads", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("forbidden: thing not enrolled"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	u := New(&fakeHub{srv: srv})
	_, err := u.Upload(context.Background(), "evt", "request", "application/json", []byte("body"))
	if !errors.Is(err, ErrFallbackInline) {
		t.Errorf("403 mint: not wrapped in ErrFallbackInline: %v", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error must surface upstream status: %v", err)
	}
}

func TestUpload_MintResponseMissingUploadURLFallsBack(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/internal/things/spill-uploads", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"key":     "k",
			"backend": "s3",
			// uploadUrl deliberately missing
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	u := New(&fakeHub{srv: srv})
	_, err := u.Upload(context.Background(), "evt", "request", "application/json", []byte("body"))
	if !errors.Is(err, ErrFallbackInline) {
		t.Errorf("missing uploadUrl: %v", err)
	}
}

func TestUpload_MintResponseMissingKeyFallsBack(t *testing.T) {
	var putCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/internal/things/spill-uploads", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"uploadUrl": "http://" + r.Host + "/upload",
			// key deliberately missing
		})
	})
	mux.HandleFunc("/upload", func(_ http.ResponseWriter, _ *http.Request) {
		putCalls.Add(1)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	u := New(&fakeHub{srv: srv})
	_, err := u.Upload(context.Background(), "evt", "request", "application/json", []byte("body"))
	if !errors.Is(err, ErrFallbackInline) {
		t.Errorf("missing key: %v", err)
	}
	if putCalls.Load() != 0 {
		t.Errorf("PUT must not happen when mint validation failed")
	}
}

func TestUpload_PutConflictTreatedAsTerminal(t *testing.T) {
	// 409 from the upload URL must surface as a fallback — never trigger a
	// retry with the same URL (the token was already consumed).
	mux := http.NewServeMux()
	mux.HandleFunc("/api/internal/things/spill-uploads", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(mintResponse{
			UploadURL: "http://" + r.Host + "/upload",
			Key:       "k",
			Backend:   "s3",
		})
	})
	mux.HandleFunc("/upload", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	u := New(&fakeHub{srv: srv})
	_, err := u.Upload(context.Background(), "evt", "request", "application/json", []byte("body"))
	if !errors.Is(err, ErrFallbackInline) {
		t.Errorf("409 put: %v", err)
	}
	if !strings.Contains(err.Error(), "409") {
		t.Errorf("error must surface 409: %v", err)
	}
}

func TestUpload_Put5xxFallsBackWithErrorBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/internal/things/spill-uploads", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(mintResponse{
			UploadURL: "http://" + r.Host + "/upload",
			Key:       "k",
			Backend:   "localfs",
		})
	})
	mux.HandleFunc("/upload", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("backend exploded"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	u := New(&fakeHub{srv: srv})
	_, err := u.Upload(context.Background(), "evt", "request", "", []byte("body"))
	if !errors.Is(err, ErrFallbackInline) {
		t.Errorf("500 put: %v", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error must surface 500: %v", err)
	}
}

func TestUpload_MintReturnsMalformedJSONFallsBack(t *testing.T) {
	// 200 OK with a body that doesn't parse as JSON — exercises the
	// decode-mint-response error branch.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{not even close to json`))
	}))
	defer srv.Close()

	u := New(&fakeHub{srv: srv})
	_, err := u.Upload(context.Background(), "evt", "request", "application/json", []byte("body"))
	if !errors.Is(err, ErrFallbackInline) {
		t.Errorf("malformed mint JSON: %v", err)
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error should mention decode failure: %v", err)
	}
}

func TestUpload_PutURLMalformedFallsBack(t *testing.T) {
	// Mint returns an unparseable URL — http.NewRequestWithContext for the
	// PUT must reject it. Exercises the build-put-request error path.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/internal/things/spill-uploads", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(mintResponse{
			UploadURL: "ht!tp://bad url with spaces\x00\x01", // unparseable
			Key:       "k",
			Backend:   "s3",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	u := New(&fakeHub{srv: srv})
	_, err := u.Upload(context.Background(), "evt", "request", "application/json", []byte("body"))
	if !errors.Is(err, ErrFallbackInline) {
		t.Errorf("malformed PUT URL: %v", err)
	}
}

// hubBadURL is a HubClient whose BaseURL contains an unparseable string,
// forcing http.NewRequestWithContext to fail on the mint step.
type hubBadURL struct{}

func (hubBadURL) BaseURL() string          { return "ht!tp://bad url with spaces\x00" }
func (hubBadURL) HTTPClient() *http.Client { return http.DefaultClient }

func TestUpload_MintURLMalformedFallsBack(t *testing.T) {
	u := New(hubBadURL{})
	_, err := u.Upload(context.Background(), "evt", "request", "application/json", []byte("body"))
	if !errors.Is(err, ErrFallbackInline) {
		t.Errorf("malformed mint URL: %v", err)
	}
}

func TestUpload_PutURLUnreachableFallsBack(t *testing.T) {
	// Mint returns a URL pointing at a closed port — the PUT will fail to
	// dial. Exercises the "send put request" error path.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/internal/things/spill-uploads", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(mintResponse{
			UploadURL: "http://127.0.0.1:1/upload", // port 1 is unbound on macOS/Linux
			Key:       "k",
			Backend:   "s3",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	u := New(&fakeHub{srv: srv})
	_, err := u.Upload(ctx, "evt", "request", "application/json", []byte("body"))
	if !errors.Is(err, ErrFallbackInline) {
		t.Errorf("unreachable PUT: %v", err)
	}
	if !strings.Contains(err.Error(), "put") {
		t.Errorf("error should mention put: %v", err)
	}
}

func TestUpload_ContextCancelDuringMintFallsBack(t *testing.T) {
	// Slow mint endpoint + canceled context must surface as ErrFallbackInline.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	u := New(&fakeHub{srv: srv})
	_, err := u.Upload(ctx, "evt", "request", "application/json", []byte("body"))
	if !errors.Is(err, ErrFallbackInline) {
		t.Errorf("ctx cancel: %v", err)
	}
}

func TestUpload_HubBaseURLWithTrailingSlash(t *testing.T) {
	// BaseURL ending in "/" must NOT yield a double-slash path that
	// some mux strict-matchers (and S3 paths) reject.
	var seenPath atomic.Pointer[string]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		seenPath.Store(&p)
		_ = json.NewEncoder(w).Encode(mintResponse{
			UploadURL: "http://" + r.Host + "/upload",
			Key:       "k",
			Backend:   "s3",
		})
	}))
	defer srv.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/upload", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	hub := &trailingSlashHub{base: srv.URL + "/", client: srv.Client()}
	u := New(hub)
	_, _ = u.Upload(context.Background(), "evt", "request", "", []byte("body"))
	if p := seenPath.Load(); p == nil || strings.HasPrefix(*p, "//") {
		t.Errorf("trailing-slash BaseURL produced double-slash path: %q", *p)
	}
}

type trailingSlashHub struct {
	base   string
	client *http.Client
}

func (h *trailingSlashHub) BaseURL() string          { return h.base }
func (h *trailingSlashHub) HTTPClient() *http.Client { return h.client }

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Ensure mintRequest / mintResponse stay JSON-shaped — a silent
// rename of the struct tags would break the Hub contract.
func TestMintRequest_JSONFieldStability(t *testing.T) {
	req := mintRequest{EventID: "e", Direction: "request", SizeBytes: 42, ContentType: "x", SHA256: "ab"}
	b, _ := json.Marshal(req)
	for _, want := range []string{`"eventId":"e"`, `"direction":"request"`, `"sizeBytes":42`, `"contentType":"x"`, `"sha256":"ab"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing field in JSON: %q\nfull: %s", want, b)
		}
	}
}

func TestMintResponse_JSONFieldStability(t *testing.T) {
	raw := []byte(fmt.Sprintf(`{"uploadUrl":"u","key":"k","backend":"s3","expiresAt":%q}`,
		time.Now().UTC().Format(time.RFC3339Nano)))
	var resp mintResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.UploadURL != "u" || resp.Key != "k" || resp.Backend != "s3" {
		t.Errorf("decoded: %+v", resp)
	}
}
