package traffic

import (
	"encoding/base64"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/locator"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	pgxmock "github.com/pashagolub/pgxmock/v4"
)

// tinyPNG is a 1x1 PNG header + IHDR — enough to sniff image/png.
var artifactTinyPNG = []byte{
	0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n',
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89,
}

func expectArtifactRow(mock pgxmock.PgxPoolIface, id, path string, respBody []byte, respCT string) {
	// Derive the modality the handler now switches on from the path.
	endpointType := ""
	switch {
	case contains(path, "/images/generations"):
		endpointType = "image_generation"
	case contains(path, "/audio/speech"):
		endpointType = "tts"
	case contains(path, "/chat/completions"):
		endpointType = "chat"
	}
	mock.ExpectQuery(`COALESCE\(a.ingress_format`).
		WithArgs(id).
		WillReturnRows(pgxmock.NewRows(normalizeInputCols).AddRow(
			"openai", "m", path,
			nil, "", respBody, "",
			"", respCT, nil, nil, endpointType, "", false, false))
}

func callArtifact(t *testing.T, h *Handler, id, query string) (int, string, []byte) {
	t.Helper()
	c, rec := echoCtx(http.MethodGet, "/traffic/"+id+"/artifact"+query)
	c.SetParamNames("id")
	c.SetParamValues(id)
	if err := h.GetTrafficEventArtifact(c); err != nil {
		t.Fatalf("GetTrafficEventArtifact: %v", err)
	}
	return rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()
}

func TestGetTrafficEventArtifact_Image_ServesDecodedPNG(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	b64 := base64.StdEncoding.EncodeToString(artifactTinyPNG)
	body := []byte(`{"data":[{"b64_json":"` + b64 + `","revised_prompt":"x"}]}`)
	expectArtifactRow(mock, "img1", "/v1/images/generations", body, "application/json")

	code, ct, out := callArtifact(t, h, "img1", "")
	if code != http.StatusOK {
		t.Fatalf("status=%d want 200 (%s)", code, string(out))
	}
	if ct != "image/png" {
		t.Errorf("content-type=%q want image/png", ct)
	}
	if len(out) != len(artifactTinyPNG) {
		t.Errorf("served %d bytes want the decoded PNG (%d)", len(out), len(artifactTinyPNG))
	}
}

func TestGetTrafficEventArtifact_TTS_ServesAudio(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	audio := []byte{0xff, 0xfb, 0x90, 0x00, 0x01, 0x02}
	expectArtifactRow(mock, "tts1", "/v1/audio/speech", audio, "audio/mpeg")

	code, ct, out := callArtifact(t, h, "tts1", "")
	if code != http.StatusOK {
		t.Fatalf("status=%d want 200", code)
	}
	if ct != "audio/mpeg" {
		t.Errorf("content-type=%q want audio/mpeg", ct)
	}
	if len(out) != len(audio) {
		t.Errorf("served %d bytes want the audio (%d)", len(out), len(audio))
	}
}

func TestGetTrafficEventArtifact_ImageURLMode_409(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	body := []byte(`{"data":[{"url":"https://cdn.example/i.png"}]}`)
	expectArtifactRow(mock, "imgu", "/v1/images/generations", body, "application/json")

	code, _, out := callArtifact(t, h, "imgu", "")
	if code != http.StatusConflict {
		t.Fatalf("status=%d want 409 (%s)", code, string(out))
	}
	if !contains(string(out), "cdn.example") {
		t.Errorf("409 body must carry the url: %s", out)
	}
}

func TestGetTrafficEventArtifact_ChatKind_404(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	expectArtifactRow(mock, "chat1", "/v1/chat/completions", []byte(`{"choices":[]}`), "application/json")

	code, _, _ := callArtifact(t, h, "chat1", "")
	if code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 (no inline artifact)", code)
	}
}

func TestGetTrafficEventArtifact_NoBody_404(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	expectArtifactRow(mock, "empty1", "/v1/images/generations", nil, "application/json")

	code, _, _ := callArtifact(t, h, "empty1", "")
	if code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 (no captured body)", code)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestGetTrafficEventArtifact_TTS_MislabeledCT_SniffsAudio(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	// MPEG/ADTS frame sync; the stored content-type is the (wrong) JSON envelope.
	audio := []byte{0xff, 0xfb, 0x90, 0x00, 0x01, 0x02}
	expectArtifactRow(mock, "ttsj", "/v1/audio/speech", audio, "application/json")

	code, ct, _ := callArtifact(t, h, "ttsj", "")
	if code != http.StatusOK {
		t.Fatalf("status=%d want 200", code)
	}
	if ct != "audio/mpeg" {
		t.Errorf("content-type=%q want audio/mpeg (sniffed past the mislabeled JSON CT)", ct)
	}
}

// An image body whose bytes match no known signature serves as
// application/octet-stream — never an active content-type — with nosniff so
// the browser cannot re-interpret it.
//
// The name this test used to carry claimed it covered an HTML/SVG polyglot.
// It did not: bare markup matches no signature, so it took the same path as
// any unknown blob. A REAL polyglot goes the other way — a file that is a
// valid GIF *and* valid HTML sniffs as image/gif and IS served inline — which
// TestArtifactPolyglotServesAsItsContainer covers, with the reasoning for why
// that is the safe answer.
func TestGetTrafficEventArtifact_NonImageBytes_DegradesToOctet(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	html := base64.StdEncoding.EncodeToString([]byte("<html><script>alert(1)</script></html>"))
	body := []byte(`{"data":[{"b64_json":"` + html + `"}]}`)
	expectArtifactRow(mock, "polyglot", "/v1/images/generations", body, "application/json")

	c, rec := echoCtx(http.MethodGet, "/traffic/polyglot/artifact")
	c.SetParamNames("id")
	c.SetParamValues("polyglot")
	if err := h.GetTrafficEventArtifact(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("content-type=%q want application/octet-stream (non-image must not serve as an active type)", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("missing nosniff header — the browser could sniff the bytes into HTML")
	}
	if rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Errorf("missing Cache-Control private, no-store")
	}
}

func TestSniffImageMime(t *testing.T) {
	cases := []struct {
		name string
		b    []byte
		want string
	}{
		{"png", artifactTinyPNG, "image/png"},
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0}, "image/jpeg"},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBP"), "image/webp"},
		{"gif", []byte("GIF89a\x00"), "image/gif"},
		{"unknown", []byte("plain text bytes here"), "application/octet-stream"},
	}
	for _, c := range cases {
		if got := locator.SniffMime(c.b); got != c.want {
			t.Errorf("%s: sniff=%s want %s", c.name, got, c.want)
		}
	}
}

// Kept as a smoke check on the endpoint's use of the shared sniffer. The
// authority is transport/normalize/locator/testdata/sniff-vectors.json,
// which both this program's sniffers assert against.
func TestSniffAudioMime(t *testing.T) {
	cases := []struct {
		name string
		b    []byte
		want string
	}{
		{"ogg", []byte("OggS\x00\x00"), "audio/ogg"},
		{"wav", []byte("RIFF\x00\x00\x00\x00WAVE"), "audio/wav"},
		{"flac", []byte("fLaC\x00"), "audio/flac"},
		{"id3", []byte("ID3\x04\x00"), "audio/mpeg"},
		{"adts", []byte{0xFF, 0xF1, 0x00}, "audio/mpeg"},
		// Unrecognisable bytes are NOT guessed at. The TTS route supplies
		// audio/mpeg as a fallback because it knows what it serves; the
		// sniffer itself never invents a type from nothing.
		{"unrecognised", []byte{0x00, 0x01}, "application/octet-stream"},
	}
	for _, c := range cases {
		if got := locator.SniffMime(c.b); got != c.want {
			t.Errorf("%s: sniff=%s want %s", c.name, got, c.want)
		}
	}
}

func TestGetTrafficEventArtifact_BadBase64_422(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	body := []byte(`{"data":[{"b64_json":"@@@not-base64@@@"}]}`)
	expectArtifactRow(mock, "badb64", "/v1/images/generations", body, "application/json")
	code, _, _ := callArtifact(t, h, "badb64", "")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want 422", code)
	}
}

func TestGetTrafficEventArtifact_IndexOutOfRange_404(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	body := []byte(`{"data":[{"b64_json":"aaaa"}]}`)
	expectArtifactRow(mock, "oob", "/v1/images/generations", body, "application/json")
	code, _, _ := callArtifact(t, h, "oob", "?index=5")
	if code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 (no image at index 5)", code)
	}
}

func TestGetTrafficEventArtifact_RowNotFound_404(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	// The normalize query returns no rows → Found=false → 404.
	mock.ExpectQuery(`COALESCE\(a.ingress_format`).WithArgs("ghost").
		WillReturnError(pgx.ErrNoRows)
	code, _, _ := callArtifact(t, h, "ghost", "")
	if code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 (row absent)", code)
	}
}

// A polyglot — bytes that are simultaneously a valid GIF and valid HTML — is
// the case the sniffer actually has to answer, and it answers "GIF".
//
// That is correct, and worth writing down so the next reader does not
// "harden" it into a refusal. The threat is a browser EXECUTING captured
// bytes. Serving image/gif does not create an executing context: nosniff
// forbids re-interpretation, the CSP is `default-src 'none'; sandbox` with
// `frame-ancestors 'none'`, and CORP keeps the response in this origin. The
// markup half is inert under every one of those.
//
// Refusing to serve it, or serving it as octet-stream, would instead break
// the honest case: a real GIF that happens to contain a `<` byte would stop
// previewing for a reason no reader could see.
func TestArtifactPolyglotServesAsItsContainer(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	polyglot := base64.StdEncoding.EncodeToString(
		[]byte("GIF89a<html><script>alert(1)</script></html>"))
	body := []byte(`{"data":[{"b64_json":"` + polyglot + `"}]}`)
	expectArtifactRow(mock, "poly", "/v1/images/generations", body, "application/json")

	rec := callArtifactRec(t, h, "poly", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/gif" {
		t.Fatalf("content-type=%q — the bytes ARE a gif and must be named as one", ct)
	}
	// And the containment that makes that safe must be present on this very
	// response, not assumed from another test.
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("a polyglot served inline without nosniff is re-interpretable")
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "sandbox"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP %q is missing %q — the markup half is only inert because of it", csp, want)
		}
	}
}

// The TTS fallback: an audio body with NO recognisable container header.
//
// Both existing TTS tests feed bytes starting 0xFF 0xFB, which the sniffer
// resolves through the MPEG frame-sync branch — so they passed without the
// route's fallback ever running, and deleting it left them green. A
// headerless payload (raw PCM from `response_format=pcm`, or an MP3 whose
// first frame is not at byte zero) is what actually reaches it.
func TestArtifactTTSFallbackNamesTheAudioTheRouteGuarantees(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	// Deliberately unrecognisable: no ID3, no frame sync, no container magic.
	raw := make([]byte, 64)
	for i := range raw {
		raw[i] = byte(i % 7)
	}
	expectArtifactRow(mock, "pcm", "/v1/audio/speech", raw, "application/json")

	rec := callArtifactRec(t, h, "pcm", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("content-type=%q — the route serves speech and nothing else, which is OUR knowledge, not the provider's declaration", ct)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, ".mp3") || !strings.HasPrefix(cd, "inline;") {
		t.Fatalf("Content-Disposition = %q, want an inline .mp3", cd)
	}
}
