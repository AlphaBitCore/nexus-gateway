package traffic

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/tidwall/gjson"
)

// artifactMimeAllowlist restricts the served Content-Type to inline-renderable
// image/audio types. Anything else (or an off-list sniff) serves as
// application/octet-stream so the browser downloads rather than executes it —
// a captured provider body must never render as active content (html/svg/js).
var artifactMimeAllowlist = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true,
	"audio/mpeg": true, "audio/mp3": true, "audio/wav": true, "audio/x-wav": true,
	"audio/ogg": true, "audio/webm": true, "audio/aac": true, "audio/flac": true,
	"audio/opus": true, "audio/mp4": true, "audio/x-m4a": true,
}

// GetTrafficEventArtifact streams the captured multimodal artifact bytes for a
// traffic event so the Control Plane UI can render it inline (<img>/<audio>).
// It is gated by the same read action as the other traffic-detail endpoints
// (no new IAM resource — it reads the same traffic-log resource).
//
//   - image_generation (/v1/images/generations): decodes data[index].b64_json
//     and serves it with the byte-sniffed image mime; a url-mode image (no b64)
//     → 409 carrying the URL as text (the gateway never dereferences it, and
//     the UI renders the link inert). index defaults to 0.
//   - tts (/v1/audio/speech): the whole response body IS the audio; served with
//     the captured content-type (allow-listed).
//   - every other kind (chat/embeddings/stt/video/guardrail/realtime): 404 —
//     no inline artifact (the stt audio is the uncaptured request; video
//     content is fetched through the gateway content route; guardrail never
//     captures).
func (h *Handler) GetTrafficEventArtifact(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")

	in, err := h.traffic.GetTrafficEventForNormalize(ctx, id)
	if err != nil {
		h.logger.Error("get traffic event for artifact", "trafficEventId", id, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if in == nil || !in.Found {
		return c.JSON(http.StatusNotFound, errJSON("Traffic event not found", "not_found", ""))
	}
	// Recover a spilled response body (large image/audio) verbatim.
	h.fillSpilledBodies(ctx, id, in)
	if len(in.ResponseBody) == 0 {
		return c.JSON(http.StatusNotFound, errJSON("No captured artifact for this event", "not_found", ""))
	}

	// Switch on the canonical modality column, not a path heuristic, so the
	// backend and the UI (which keys on endpointType) agree on the field.
	switch in.EndpointType {
	case "image_generation":
		return h.serveImageArtifact(c, in.ResponseBody)
	case "tts":
		return h.serveAudioArtifact(c, in.ResponseBody, in.ResponseContentType)
	default:
		return c.JSON(http.StatusNotFound, errJSON("This event has no inline artifact to preview", "not_found", ""))
	}
}

func (h *Handler) serveImageArtifact(c echo.Context, body []byte) error {
	index := 0
	if v := c.QueryParam("index"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			index = n
		}
	}
	item := gjson.GetBytes(body, "data."+strconv.Itoa(index))
	b64 := item.Get("b64_json").String()
	if b64 == "" {
		if url := item.Get("url").String(); url != "" {
			// URL-mode: the gateway never fetched the image; hand the caller the
			// reference so the UI renders an inert link.
			return c.JSON(http.StatusConflict, map[string]string{
				"code": "artifact_is_url", "url": url,
			})
		}
		return c.JSON(http.StatusNotFound, errJSON("No image artifact at that index", "not_found", ""))
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return c.JSON(http.StatusUnprocessableEntity, errJSON("Artifact is not decodable base64", "unprocessable", ""))
	}
	return serveArtifactBytes(c, raw, sniffImageMime(raw))
}

func (h *Handler) serveAudioArtifact(c echo.Context, body []byte, contentType string) error {
	// The path is gated to /v1/audio/speech, so the body IS audio. Trust the
	// stored content-type only when it is already an allow-listed audio type —
	// capture often mislabels the TTS response content-type (e.g.
	// application/json, the JSON envelope CT), which would strand the audio as
	// octet-stream and break the <audio> player. Otherwise sniff the container
	// magic bytes, defaulting to audio/mpeg (the OpenAI TTS default).
	mime := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if !artifactMimeAllowlist[mime] || !strings.HasPrefix(mime, "audio/") {
		mime = sniffAudioMime(body)
	}
	return serveArtifactBytes(c, body, mime)
}

// sniffAudioMime recognizes the common TTS audio containers by magic bytes,
// defaulting to audio/mpeg since the caller has already established (by route)
// that the bytes are speech audio.
func sniffAudioMime(b []byte) string {
	switch {
	case len(b) >= 4 && string(b[:4]) == "OggS":
		return "audio/ogg"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WAVE":
		return "audio/wav"
	case len(b) >= 4 && string(b[:4]) == "fLaC":
		return "audio/flac"
	case len(b) >= 3 && string(b[:3]) == "ID3":
		return "audio/mpeg"
	case len(b) >= 2 && b[0] == 0xFF && (b[1]&0xE0) == 0xE0:
		return "audio/mpeg" // MPEG/ADTS frame sync
	}
	return "audio/mpeg"
}

// serveArtifactBytes writes the artifact with an allow-listed content-type,
// nosniff, and inline disposition. An off-list mime degrades to
// application/octet-stream so a captured body can never render as active
// content in the admin browser.
func serveArtifactBytes(c echo.Context, raw []byte, mime string) error {
	if !artifactMimeAllowlist[mime] {
		mime = "application/octet-stream"
	}
	c.Response().Header().Set("X-Content-Type-Options", "nosniff")
	c.Response().Header().Set("Content-Disposition", "inline")
	// Captured provider content can be sensitive — keep it out of shared/proxy
	// caches and off disk.
	c.Response().Header().Set("Cache-Control", "private, no-store")
	return c.Blob(http.StatusOK, mime, raw)
}

// sniffImageMime returns the image mime for common web-image magic bytes,
// defaulting to application/octet-stream (never an active type).
func sniffImageMime(b []byte) string {
	switch {
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	case len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "image/gif"
	}
	return "application/octet-stream"
}
