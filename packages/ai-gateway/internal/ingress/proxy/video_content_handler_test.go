package proxy

// video_content_handler_test.go — business-behavior tests for
// ServeVideoContent (e88-s6 T7): byte-identical streamed relay + fingerprint,
// the Content-Type allowlist refusal, the 1 GiB ceiling (declared → clean
// 502; mid-stream → connection abort, never a silent short file), the
// variant-only query passthrough, and the shared owned-lookup admission.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/generativecaps"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// videoContentUpstream serves a canned artifact body with a given content
// type, recording the raw RequestURI.
func videoContentUpstream(t *testing.T, contentType string, body []byte) *followUpstream {
	t.Helper()
	f := &followUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits++
		f.method = r.Method
		f.uri = r.RequestURI
		f.mu.Unlock()
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// A complete relay is byte-identical, carries the D-V5 hygiene headers, and
// stamps the artifact fingerprint of exactly the bytes the client received on
// a $0, coverage=none audit row.
func TestServeVideoContent_StreamsAndFingerprints(t *testing.T) {
	artifact := []byte("MP4-BYTES-" + strings.Repeat("x", 4096))
	up := videoContentUpstream(t, "video/mp4", artifact)
	deps, _, prod, _ := videoFollowDeps(t, up.srv.URL)
	h := NewHandler(deps).ServeVideoContent(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, "video_abc")
	if rr.Code != http.StatusOK || !strings.HasPrefix(rr.Body.String(), "MP4-BYTES-") || rr.Body.Len() != len(artifact) {
		t.Fatalf("relay = %d, %d bytes; want 200 with the %d-byte artifact", rr.Code, rr.Body.Len(), len(artifact))
	}
	if up.uri != "/v1/videos/video_abc/content" {
		t.Errorf("upstream RequestURI = %q, want /v1/videos/video_abc/content", up.uri)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("Content-Type = %q, want video/mp4", ct)
	}
	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("nosniff missing (got %q)", got)
	}
	if got := rr.Header().Get("Content-Disposition"); got != `attachment; filename="video_abc.mp4"` {
		t.Errorf("Content-Disposition = %q, want attachment with the derived filename", got)
	}

	sum := sha256.Sum256(artifact)
	wantRefs := fmt.Sprintf(`[{"sha256":%q,"sizeBytes":%d,"mime":"video/mp4"}]`,
		hex.EncodeToString(sum[:]), len(artifact))
	msg := lastAudit(t, deps, prod)
	if msg.ArtifactRefs != wantRefs {
		t.Errorf("artifact_refs = %s, want %s", msg.ArtifactRefs, wantRefs)
	}
	if msg.EstimatedCostUsd != 0 {
		t.Errorf("content row cost = %v, want $0 (single cost-bearing row is the submit row)", msg.EstimatedCostUsd)
	}
	if msg.ComplianceCoverage != "none" {
		t.Errorf("coverage = %q, want none (the artifact is not content-scanned — R-1)", msg.ComplianceCoverage)
	}
	if msg.EndpointType != string(typology.EndpointKindVideoGeneration) {
		t.Errorf("endpoint_type = %q, want video_generation", msg.EndpointType)
	}
	if msg.ErrorCode != nil {
		t.Errorf("error_code = %v, want nil on a complete relay", *msg.ErrorCode)
	}
}

// Only the `variant` parameter passes to the provider; everything else in the
// client's query string is dropped (no query smuggling through the relay).
func TestServeVideoContent_VariantOnlyQueryPassthrough(t *testing.T) {
	up := videoContentUpstream(t, "image/jpeg", []byte("JPEG"))
	deps, _, _, _ := videoFollowDeps(t, up.srv.URL)
	h := NewHandler(deps).ServeVideoContent(videoIngress())

	req := httptest.NewRequest(http.MethodGet, "/v1/videos/video_abc/content?variant=thumbnail&evil=1&x=%2e%2e", nil)
	req.SetPathValue("id", "video_abc")
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if up.uri != "/v1/videos/video_abc/content?variant=thumbnail" {
		t.Errorf("upstream RequestURI = %q, want only the variant param forwarded", up.uri)
	}
}

// A 2xx artifact outside the Content-Type allowlist is refused before any
// byte reaches the client — a compromised provider must not serve active
// content through the authenticated gateway origin.
func TestServeVideoContent_ContentTypeRejected(t *testing.T) {
	up := videoContentUpstream(t, "text/html; charset=utf-8", []byte("<script>alert(1)</script>"))
	deps, _, _, _ := videoFollowDeps(t, up.srv.URL)
	h := NewHandler(deps).ServeVideoContent(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, "video_abc")
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "VIDEO_CONTENT_TYPE_REJECTED") {
		t.Fatalf("resp = %d %q, want 502 VIDEO_CONTENT_TYPE_REJECTED", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "<script>") {
		t.Errorf("provider body leaked through the refusal")
	}
}

// Content-Type parameters must not dodge the allowlist match.
func TestServeVideoContent_ContentTypeParamsAllowed(t *testing.T) {
	up := videoContentUpstream(t, `video/mp4; codecs="avc1.42E01E"`, []byte("MP4"))
	deps, _, _, _ := videoFollowDeps(t, up.srv.URL)
	h := NewHandler(deps).ServeVideoContent(videoIngress())
	if rr := doVideoFollow(h, http.MethodGet, "video_abc"); rr.Code != http.StatusOK {
		t.Errorf("parameterized video/mp4 → %d, want 200", rr.Code)
	}
}

// stubBody yields its payload then a configurable terminal error (io.EOF for
// a clean end).
type stubBody struct {
	data []byte
	err  error
}

func (s *stubBody) Read(p []byte) (int, error) {
	if len(s.data) == 0 {
		return 0, s.err
	}
	n := copy(p, s.data)
	s.data = s.data[n:]
	return n, nil
}
func (s *stubBody) Close() error { return nil }

// stubContentTransport fabricates upstream responses (a declared oversize
// Content-Length can't be produced by a real httptest server).
type stubContentTransport struct {
	status        int
	contentType   string
	contentLength int64
	body          io.ReadCloser
}

func (s stubContentTransport) RoundTrip(*http.Request) (*http.Response, error) {
	h := http.Header{}
	h.Set("Content-Type", s.contentType)
	return &http.Response{
		StatusCode:    s.status,
		Header:        h,
		ContentLength: s.contentLength,
		Body:          s.body,
	}, nil
}

// A DECLARED oversize artifact fails as a clean 502 before any byte is
// written.
func TestServeVideoContent_DeclaredOversize502(t *testing.T) {
	deps, _, _, _ := videoFollowDeps(t, "http://irrelevant")
	deps.UpstreamClient = &http.Client{Transport: stubContentTransport{
		status: http.StatusOK, contentType: "video/mp4",
		contentLength: videoContentMaxBytes + 1,
		body:          &stubBody{data: []byte("x"), err: io.EOF},
	}}
	h := NewHandler(deps).ServeVideoContent(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, "video_abc")
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "VIDEO_CONTENT_TOO_LARGE") {
		t.Fatalf("resp = %d %q, want 502 VIDEO_CONTENT_TOO_LARGE", rr.Code, rr.Body.String())
	}
}

// A MID-STREAM overflow (undeclared length crossing the ceiling) aborts the
// connection — the client must see a failed transfer, never a plausible
// truncated file — and stamps no fingerprint.
func TestServeVideoContent_MidStreamOverflowAborts(t *testing.T) {
	old := videoContentMaxBytes
	videoContentMaxBytes = 1024
	t.Cleanup(func() { videoContentMaxBytes = old })

	deps, _, prod, _ := videoFollowDeps(t, "http://irrelevant")
	deps.UpstreamClient = &http.Client{Transport: stubContentTransport{
		status: http.StatusOK, contentType: "video/mp4",
		contentLength: -1, // undeclared (chunked)
		body:          &stubBody{data: []byte(strings.Repeat("z", 5000)), err: io.EOF},
	}}
	h := NewHandler(deps).ServeVideoContent(videoIngress())

	req := httptest.NewRequest(http.MethodGet, "/v1/videos/video_abc/content", nil)
	req.SetPathValue("id", "video_abc")
	rr := httptest.NewRecorder()
	var panicked any
	func() {
		defer func() { panicked = recover() }()
		h(rr, req)
	}()
	panickedErr, _ := panicked.(error)
	if !errors.Is(panickedErr, http.ErrAbortHandler) {
		t.Fatalf("panic = %v, want http.ErrAbortHandler (the honest abort once the status line is committed)", panicked)
	}
	if got := rr.Body.Len(); int64(got) != 1024 {
		t.Errorf("client received %d bytes, want exactly the ceiling (1024)", got)
	}
	// No fingerprint for an aborted transfer — artifact_refs records only
	// what a client fully received — and the row carries the abort
	// classification (a bare 200-with-empty-refs could not distinguish
	// overflow from a client disconnect).
	msg := lastAudit(t, deps, prod)
	if msg.ArtifactRefs != "" {
		t.Errorf("artifact_refs = %s, want empty on an aborted relay", msg.ArtifactRefs)
	}
	if msg.ErrorCode == nil || *msg.ErrorCode != "VIDEO_CONTENT_RELAY_ABORTED" {
		t.Errorf("error_code = %v, want VIDEO_CONTENT_RELAY_ABORTED", msg.ErrorCode)
	}
}

// An upstream body error mid-stream ends the relay without a panic and
// without a fingerprint (the copy already stopped; nothing more can be
// honestly written).
func TestServeVideoContent_UpstreamResetMidStream(t *testing.T) {
	deps, _, prod, _ := videoFollowDeps(t, "http://irrelevant")
	deps.UpstreamClient = &http.Client{Transport: stubContentTransport{
		status: http.StatusOK, contentType: "video/mp4",
		contentLength: -1,
		body:          &stubBody{data: []byte("partial"), err: errors.New("upstream reset")},
	}}
	h := NewHandler(deps).ServeVideoContent(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, "video_abc")
	if rr.Code != http.StatusOK || rr.Body.String() != "partial" {
		t.Fatalf("relay = %d %q, want the partial bytes under the committed 200", rr.Code, rr.Body.String())
	}
	msg := lastAudit(t, deps, prod)
	if msg.ArtifactRefs != "" {
		t.Errorf("artifact_refs = %s, want empty on an incomplete relay", msg.ArtifactRefs)
	}
	if msg.ErrorCode == nil || *msg.ErrorCode != "VIDEO_CONTENT_RELAY_INCOMPLETE" {
		t.Errorf("error_code = %v, want VIDEO_CONTENT_RELAY_INCOMPLETE", msg.ErrorCode)
	}
}

// A declared in-budget Content-Length relays to the client (it can detect an
// interrupted transfer against it).
func TestServeVideoContent_DeclaredLengthRelayed(t *testing.T) {
	deps, _, _, _ := videoFollowDeps(t, "http://irrelevant")
	deps.UpstreamClient = &http.Client{Transport: stubContentTransport{
		status: http.StatusOK, contentType: "video/mp4",
		contentLength: 3,
		body:          &stubBody{data: []byte("MP4"), err: io.EOF},
	}}
	h := NewHandler(deps).ServeVideoContent(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, "video_abc")
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Length") != "3" {
		t.Errorf("relay = %d CL=%q, want 200 with Content-Length 3", rr.Code, rr.Header().Get("Content-Length"))
	}
}

// An unreachable upstream is a 502 before any artifact byte.
func TestServeVideoContent_UpstreamUnreachable502(t *testing.T) {
	deps, _, _, _ := videoFollowDeps(t, "http://127.0.0.1:1")
	h := NewHandler(deps).ServeVideoContent(videoIngress())
	rr := doVideoFollow(h, http.MethodGet, "video_abc")
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "UPSTREAM_ERROR") {
		t.Fatalf("resp = %d %q, want 502 UPSTREAM_ERROR", rr.Code, rr.Body.String())
	}
}

// Provider errors (expired artifact, unknown variant) relay verbatim through
// the buffered small-body path.
func TestServeVideoContent_ProviderErrorRelaysVerbatim(t *testing.T) {
	body := `{"error":{"message":"video expired","type":"invalid_request_error"}}`
	up := newFollowUpstream(t, http.StatusNotFound, body)
	deps, _, _, _ := videoFollowDeps(t, up.srv.URL)
	h := NewHandler(deps).ServeVideoContent(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, "video_abc")
	if rr.Code != http.StatusNotFound || rr.Body.String() != body {
		t.Fatalf("relay = %d %q, want provider 404 verbatim", rr.Code, rr.Body.String())
	}
}

// The content route shares the owned-lookup admission: unknown id → 404
// non-disclosure with the upstream never probed; expired row → 410.
func TestServeVideoContent_NotFoundAndExpired(t *testing.T) {
	up := videoContentUpstream(t, "video/mp4", []byte("MP4"))
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	js.owned = nil
	h := NewHandler(deps).ServeVideoContent(videoIngress())
	if rr := doVideoFollow(h, http.MethodGet, "video_x"); rr.Code != http.StatusNotFound {
		t.Errorf("unknown id → %d, want 404", rr.Code)
	}

	deps2, js2, _, _ := videoFollowDeps(t, up.srv.URL)
	js2.owned.Status = "expired"
	h2 := NewHandler(deps2).ServeVideoContent(videoIngress())
	if rr := doVideoFollow(h2, http.MethodGet, "video_abc"); rr.Code != http.StatusGone {
		t.Errorf("expired row → %d, want 410", rr.Code)
	}
	if up.hits != 0 {
		t.Errorf("upstream hits = %d, want 0", up.hits)
	}
}

// ceilingReader semantics: exactly `remaining` bytes flow, then the explicit
// overflow sentinel — an EOF inside the budget is a clean end, and a body of
// EXACTLY the budget relays cleanly even when the source splits its final EOF
// into a separate (0, io.EOF) read (io.Reader's contract permits either shape
// — the exact-size case must not depend on stdlib coalescing).
func TestCeilingReader(t *testing.T) {
	c := &ceilingReader{r: strings.NewReader("abcdef"), remaining: 4}
	got, err := io.ReadAll(c)
	if !errors.Is(err, errVideoContentOverflow) || string(got) != "abcd" {
		t.Errorf("overflow case = (%q, %v), want (abcd, errVideoContentOverflow)", got, err)
	}

	c2 := &ceilingReader{r: strings.NewReader("ab"), remaining: 10}
	got2, err2 := io.ReadAll(c2)
	if err2 != nil || string(got2) != "ab" {
		t.Errorf("within-budget case = (%q, %v), want (ab, nil)", got2, err2)
	}

	// Exactly at the budget, split EOF (stubBody returns data then a separate
	// (0, io.EOF)) — must be a clean end, not a spurious overflow.
	c3 := &ceilingReader{r: &stubBody{data: []byte("wxyz"), err: io.EOF}, remaining: 4}
	got3, err3 := io.ReadAll(c3)
	if err3 != nil || string(got3) != "wxyz" {
		t.Errorf("exact-budget split-EOF case = (%q, %v), want (wxyz, nil)", got3, err3)
	}
}

// An artifact of EXACTLY the ceiling relays cleanly end-to-end and stamps its
// fingerprint (three review lenses flagged the boundary as stdlib-coalescing-
// dependent; the probe-read fix pins it).
func TestServeVideoContent_ExactCeilingRelaysClean(t *testing.T) {
	old := videoContentMaxBytes
	videoContentMaxBytes = 2048
	t.Cleanup(func() { videoContentMaxBytes = old })

	exact := []byte(strings.Repeat("v", 2048))
	deps, _, prod, _ := videoFollowDeps(t, "http://irrelevant")
	deps.UpstreamClient = &http.Client{Transport: stubContentTransport{
		status: http.StatusOK, contentType: "video/mp4",
		contentLength: -1, // chunked — the ceilingReader owns the bound
		body:          &stubBody{data: exact, err: io.EOF},
	}}
	h := NewHandler(deps).ServeVideoContent(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, "video_abc")
	if rr.Code != http.StatusOK || rr.Body.Len() != 2048 {
		t.Fatalf("relay = %d, %d bytes; want a clean 200 with all 2048", rr.Code, rr.Body.Len())
	}
	msg := lastAudit(t, deps, prod)
	if msg.ArtifactRefs == "" {
		t.Errorf("artifact_refs empty — an exact-ceiling artifact is a COMPLETE relay and must fingerprint")
	}
}

// The per-(VK, video) generative-caps concurrency bounds parallel transfers:
// with the cap saturated by an in-flight download, the next request sheds 429
// with Retry-After — one key cannot pin unbounded global admission slots for
// GiB-long streams.
func TestServeVideoContent_PerVKConcurrencyCap(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce, firstOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	firstIn := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("MP4"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		firstOnce.Do(func() { close(firstIn) })
		<-release // hold the transfer open so the VK's slots stay taken
	}))
	t.Cleanup(func() { releaseAll(); up.Close() })

	deps, _, _, _ := videoFollowDeps(t, up.URL)
	h := NewHandler(deps).ServeVideoContent(videoIngress())

	caps, _ := generativecaps.Lookup(typology.EndpointKindVideoGeneration)
	if caps.MaxConcurrentPerVK <= 0 {
		t.Fatal("video gencaps row lost its concurrency bound")
	}
	done := make(chan struct{})
	for range caps.MaxConcurrentPerVK {
		go func() {
			defer func() { done <- struct{}{} }()
			doVideoFollow(h, http.MethodGet, "video_abc")
		}()
	}
	<-firstIn // at least one transfer is holding; the rest are in flight

	// Saturation is racy from outside (the holders may still be resolving),
	// so poll the shed response briefly: within the window one extra request
	// must eventually see 429 GENERATIVE_CONCURRENCY_LIMIT.
	//
	// A probe that lands BEFORE the holders have taken every slot is admitted
	// and then blocks streaming the upstream body, which nothing releases
	// until this loop ends — so the probe must not be run inline, or the test
	// wedges until the go test timeout instead of failing. Run each probe on
	// its own goroutine and treat "still in flight" as "not shed yet": an
	// admitted probe holds a slot, which only brings the next probe closer to
	// the shed this asserts. Every probe drains once releaseAll fires.
	probe := func() *httptest.ResponseRecorder {
		ch := make(chan *httptest.ResponseRecorder, 1)
		go func() { ch <- doVideoFollow(h, http.MethodGet, "video_abc") }()
		select {
		case rr := <-ch:
			return rr
		case <-time.After(200 * time.Millisecond):
			return nil // admitted, now streaming — keep polling
		}
	}
	deadline := time.After(5 * time.Second)
	for {
		rr := probe()
		if rr != nil && rr.Code == http.StatusTooManyRequests &&
			strings.Contains(rr.Body.String(), "GENERATIVE_CONCURRENCY_LIMIT") &&
			rr.Header().Get("Retry-After") != "" {
			break
		}
		select {
		case <-deadline:
			if rr == nil {
				t.Fatal("never saw the 429 shed; the last probe was still admitted and streaming")
			}
			t.Fatalf("never saw the 429 shed; last = %d %q", rr.Code, rr.Body.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
	releaseAll()
	for range caps.MaxConcurrentPerVK {
		<-done
	}
}

// The suggested download filename derives from the job id + validated type;
// a provider-controlled id with header-hostile characters degrades to a bare
// attachment (never reaches a header value).
func TestVideoContentDisposition(t *testing.T) {
	for _, tc := range []struct{ id, base, want string }{
		{"video_abc", "video/mp4", `attachment; filename="video_abc.mp4"`},
		{"veo_bW9kZWxz-_.", "image/png", `attachment; filename="veo_bW9kZWxz-_..png"`},
		{`vid"eo`, "video/mp4", "attachment"},
		{"vid eo", "video/mp4", "attachment"},
		{"", "video/mp4", "attachment"},
		{"video_abc", "application/unknown", "attachment"},
	} {
		if got := videoContentDisposition(tc.id, tc.base); got != tc.want {
			t.Errorf("disposition(%q, %q) = %q, want %q", tc.id, tc.base, got, tc.want)
		}
	}
}
