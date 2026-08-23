package traffic

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/store/trafficstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/locator"
)

// inlineRenderableMimes is the FROZEN set of types this endpoint will serve
// with an inline disposition.
//
// Frozen means: adding a row is a security decision, not a convenience one.
// Everything served here is bytes a third party put on the wire, rendered in
// a browser that is already authenticated as an administrator. An inline
// type must therefore be one the browser cannot be talked into executing —
// which rules out html, svg, xml and anything script-adjacent no matter how
// convenient a preview would be.
//
// Anything not on this list still SERVES; it just downloads instead of
// rendering. That is the whole difference, and it is why the list can stay
// short without anything becoming unreachable.
var inlineRenderableMimes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true,
	"audio/mpeg": true, "audio/wav": true, "audio/ogg": true, "audio/webm": true,
	"audio/flac": true, "audio/mp4": true, "audio/aac": true,
	"video/mp4": true, "video/webm": true,
	// NOT application/pdf. ui-shared's NEVER_INLINE_MIMES already refuses it
	// — a captured body is attacker-influenced content, and a PDF viewer is
	// a rendering context — and the server is the half that actually
	// decides, so it holds the same line. A PDF still SERVES; it downloads.
}

// GetTrafficEventArtifact serves the bytes a MediaRef locator addresses.
//
//	GET /traffic/:id/artifact?locator=<locator>&direction=request|response
//
// The locator is the one the normalized payload published, resolved by the
// same package that built it. That shared resolution is the point: before
// it, the card decided a download was available and this endpoint decided
// separately whether it was, so the two could disagree — and a card that
// offers a control which then fails is the failure this whole area exists
// to prevent.
//
// Without a locator the endpoint keeps its original behaviour for the two
// shapes that shipped with it — image generations and TTS. Those are not a
// second implementation: they derive the locator the caller did not send and
// go down the same path.
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
	// Recover a spilled body (large image/audio/video) verbatim. A locator
	// addresses the body as the wire carried it, so a spilled body must be
	// whole again before anything is resolved against it.
	h.fillSpilledBodies(ctx, id, in)

	loc := strings.TrimSpace(c.QueryParam("locator"))
	direction := strings.TrimSpace(c.QueryParam("direction"))
	if direction == "" {
		direction = "response"
	}

	var fallbackMime string
	urlModeIndex := -1
	if loc == "" {
		var status int
		loc, status, fallbackMime, urlModeIndex = h.legacyLocator(c, in)
		if status != 0 {
			return status2Response(c, status, in)
		}
		direction = "response"
	}

	var body []byte
	switch direction {
	case "response":
		body = in.ResponseBody
	case "request":
		body = in.RequestBody
	default:
		return c.JSON(http.StatusBadRequest, errJSON("direction must be request or response", "bad_request", ""))
	}
	if len(body) == 0 {
		return c.JSON(http.StatusNotFound, errJSON("No captured "+direction+" body for this event", "not_found", ""))
	}

	got, err := locator.Resolve(body, loc)
	if err != nil {
		// A url-mode image generation has no bytes to serve, and the shipped
		// contract answers that with 409 carrying the reference so the UI can
		// render it inert rather than showing a dead preview. Checked only
		// when the bytes really are missing, so a decodable image never takes
		// this path.
		if urlModeIndex >= 0 && locator.ReasonOf(err) == locator.ReasonNotFound {
			if u := gjson.GetBytes(body, "data."+strconv.Itoa(urlModeIndex)+".url").String(); u != "" {
				return c.JSON(http.StatusConflict, map[string]string{"code": "artifact_is_url", "url": u})
			}
		}
		return resolveFailure(c, err)
	}
	return serveArtifactBytes(c, id, got.Bytes, fallbackMime)
}

// legacyLocator derives the locator for the two shapes that predate the
// grammar, so the original request form keeps working byte for byte.
//
// It returns a non-zero HTTP status when the shape has no servable artifact
// at all, which the original endpoint answered with a specific code the UI
// still branches on.
// The third return is a fallback container type for bytes the sniffer does
// not recognise. It is only ever set from what the ROUTE guarantees — the
// TTS path serves speech and nothing else — never from a content-type the
// provider chose, which capture has been measured to mislabel.
// The fourth return is the image index to check for a url-mode reference
// when the bytes are absent, or -1 when the shape has no such fallback.
func (h *Handler) legacyLocator(c echo.Context, in *trafficstore.NormalizeInput) (string, int, string, int) {
	switch in.EndpointType {
	case "image_generation":
		index := 0
		if v := c.QueryParam("index"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				index = n
			}
		}
		return locator.JSON(locator.JoinSuffix(locator.JoinPath("data", index), "b64_json")), 0, "", index
	case "tts":
		// The whole response body IS the audio. An MP3 with neither an ID3
		// header nor a frame sync at byte zero sniffs as nothing, and
		// serving it as octet-stream would strand a real recording behind a
		// download when the route already established what it is.
		return locator.Body, 0, "audio/mpeg", -1
	default:
		return "", http.StatusNotFound, "", -1
	}
}

// resolveFailure maps a resolution reason to a status. The reason is
// matched, never the message: a wording change must not turn a 404 into a
// 500, and the UI branches on the code.
func resolveFailure(c echo.Context, err error) error {
	switch locator.ReasonOf(err) {
	case locator.ReasonNotFound:
		return c.JSON(http.StatusNotFound, errJSON("The locator addresses nothing in this event", "not_found", ""))
	case locator.ReasonMalformed, locator.ReasonUnsupported:
		return c.JSON(http.StatusBadRequest, errJSON("Unreadable locator", "bad_request", ""))
	case locator.ReasonUndecodable:
		// The bytes are there and are not recoverable. Saying so is the
		// honest answer; the card shows the same fact as a cause.
		return c.JSON(http.StatusUnprocessableEntity, errJSON("The captured bytes are not decodable", "unprocessable", ""))
	}
	return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
}

// status2Response answers the legacy shapes that carry no servable artifact.
func status2Response(c echo.Context, status int, in *trafficstore.NormalizeInput) error {
	if status == http.StatusNotFound && in.EndpointType == "image_generation" {
		return c.JSON(http.StatusNotFound, errJSON("No image artifact at that index", "not_found", ""))
	}
	return c.JSON(http.StatusNotFound, errJSON("This event has no inline artifact to preview", "not_found", ""))
}

// serveArtifactBytes writes the artifact under headers chosen so that a
// captured provider body can never act inside the admin browser.
//
// The served type comes from SNIFFING the bytes, never from what the wire
// declared. A declared type is written by whoever sent the payload, so
// trusting it is how a script arrives labelled as a picture.
func serveArtifactBytes(c echo.Context, eventID string, raw []byte, fallbackMime string) error {
	sniffed := locator.SniffMime(raw)
	if sniffed == "application/octet-stream" && fallbackMime != "" {
		sniffed = fallbackMime
	}
	// The served type and the download NAME are two decisions, not one.
	//
	// Serving anything off the frozen list as octet-stream is the safety
	// call. Naming the file from that blanked type is not: a HEIC photo, an
	// AVIF, a PDF are all correctly identified and then downloaded as
	// `.bin`, which no system knows how to open. The name comes from what
	// the bytes ARE; only the Content-Type is conservative.
	inline := inlineRenderableMimes[sniffed]
	mime := sniffed
	if !inline {
		mime = "application/octet-stream"
	}

	sum := sha256.Sum256(raw)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`

	hdr := c.Response().Header()
	// Never sniff: the served type is the decision made above, and letting
	// the browser second-guess it re-opens exactly what sniffing here closes.
	hdr.Set("X-Content-Type-Options", "nosniff")
	// Even for an inline type, deny the document every capability. A
	// sandboxed document with no sources cannot navigate, script, or phone
	// home if a decoder is ever talked into treating bytes as markup.
	hdr.Set("Content-Security-Policy", "default-src 'none'; sandbox; frame-ancestors 'none'")
	// Keep the bytes out of any other origin's process.
	hdr.Set("Cross-Origin-Resource-Policy", "same-origin")
	// Captured content is sensitive: no shared cache, no disk.
	hdr.Set("Cache-Control", "private, no-store")
	hdr.Set("ETag", etag)
	hdr.Set("Content-Disposition", disposition(inline, eventID, sniffed))

	// A revalidation costs nothing and saves re-sending a large artifact
	// the browser already holds.
	if match := c.Request().Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		return c.NoContent(http.StatusNotModified)
	}
	return c.Blob(http.StatusOK, mime, raw)
}

func disposition(inline bool, eventID, mime string) string {
	kind := "attachment"
	if inline {
		kind = "inline"
	}
	ext := locator.ExtensionForMime(mime)
	// The id is ours and is safe in a header; the extension comes from the
	// sniffed type, so a provider cannot choose the name a browser writes.
	return fmt.Sprintf(`%s; filename="nexus-%s.%s"`, kind, sanitizeForHeader(eventID), ext)
}

// sanitizeForHeader keeps a filename to characters that cannot terminate the
// header or escape the quoted string.
func sanitizeForHeader(s string) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < 64; i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteByte(c)
		}
	}
	if b.Len() == 0 {
		return "artifact"
	}
	return b.String()
}
