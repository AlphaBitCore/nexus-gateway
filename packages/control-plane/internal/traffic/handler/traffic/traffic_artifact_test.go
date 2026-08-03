package traffic

import (
	"encoding/base64"
	"net/http"
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
			"", respCT, nil, nil, endpointType))
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

// TestGetTrafficEventArtifact_NonImageBytes_DegradesToOctet is the load-bearing
// XSS control: an image body whose bytes are NOT a known raster type (e.g. an
// HTML/SVG polyglot) must serve as application/octet-stream — never an active
// content-type — with nosniff so the browser cannot re-interpret it.
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
		if got := sniffImageMime(c.b); got != c.want {
			t.Errorf("%s: sniff=%s want %s", c.name, got, c.want)
		}
	}
}

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
		{"default", []byte{0x00, 0x01}, "audio/mpeg"},
	}
	for _, c := range cases {
		if got := sniffAudioMime(c.b); got != c.want {
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
