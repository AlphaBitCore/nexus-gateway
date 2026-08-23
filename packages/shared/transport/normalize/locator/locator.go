// Package locator owns the MediaRef locator grammar: the constructors that
// build one, the predicates that decide whether bytes are addressable, and
// the resolver that reads them back.
//
// All three live together on purpose. A locator is a promise that bytes can
// be recovered from a stored body, and until this package existed the
// promise was made in one place (the codecs) and kept in another (the admin
// artifact endpoint and the browser resolver), each with its own idea of
// what counted as decodable. Divergence between them is not a cosmetic bug:
// it is a Download control that throws on click, which is the single failure
// this whole area is built to prevent.
//
// The grammar has five containers, and every one of them resolves from the
// stored bytes alone — no database, no session, no provider call. That is
// what makes a locator portable: the same string means the same bytes in the
// Control Plane, in the agent, and in a browser.
//
//	body                  the entire stored body IS the artifact
//	json:<path>           plain base64 at a gjson path
//	datauri:<path>        a full data URI at a gjson path
//	sse:<frame>:<path>    a gjson path inside the Nth data-bearing SSE frame
//	multipart:<part>      a named part of a multipart body
package locator

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"strconv"
	"strings"
	"unicode"

	"github.com/tidwall/gjson"
)

// Container prefixes. Exported so a caller can branch on the shape without
// re-deriving the grammar from string literals.
const (
	Body            = "body"
	prefixJSON      = "json:"
	prefixDataURI   = "datauri:"
	prefixSSE       = "sse:"
	prefixMultipart = "multipart:"
)

// Failure reasons, carried on Error so a caller can map them to a status
// code or a UI state without matching on message text.
const (
	ReasonMalformed   = "malformed"   // the locator itself does not parse
	ReasonUnsupported = "unsupported" // a container this caller cannot read
	ReasonNotFound    = "not-found"   // the locator parsed but addresses nothing
	ReasonUndecodable = "undecodable" // it addresses something that is not base64
)

// Error is a resolution failure with a machine-readable reason.
type Error struct {
	Reason  string
	Message string
}

func (e *Error) Error() string { return e.Reason + ": " + e.Message }

func failf(reason, format string, args ...any) error {
	return &Error{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// ReasonOf returns the failure reason of err, or "" if err is not a
// resolution failure.
func ReasonOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
}

// ---------------------------------------------------------------- grammar

// JoinPath appends an array index to a gjson path. An empty base yields an
// empty path, and an empty path yields an empty locator — that is how a
// codec says "these bytes exist but I cannot address them", rather than
// emitting a locator that points at nothing.
func JoinPath(base string, idx int) string {
	if base == "" {
		return ""
	}
	return base + "." + strconv.Itoa(idx)
}

// JoinSuffix appends a field name to a gjson path, with the same
// empty-base propagation as JoinPath.
func JoinSuffix(base, suffix string) string {
	if base == "" {
		return ""
	}
	return base + "." + suffix
}

// JSON addresses plain base64 at a gjson path.
func JSON(path string) string {
	if path == "" {
		return ""
	}
	return prefixJSON + path
}

// DataURI addresses a whole data URI at a gjson path.
func DataURI(path string) string {
	if path == "" {
		return ""
	}
	return prefixDataURI + path
}

// SSE addresses a gjson path inside the Nth data-bearing frame of a
// captured SSE body. Frames are counted in arrival order over data-bearing
// frames only — the same order the stream folds walk, so the index a codec
// emits and the index this package counts cannot drift.
//
// This container exists because streamed media is whole within its frame:
// Gemini delivers a complete inlineData part per chunk, and OpenAI's
// partial_image events each carry a fully renderable image rather than a
// byte fragment. Only a producer that split one payload across frames would
// be unaddressable, and none of the measured ones does.
func SSE(frame int, path string) string {
	if path == "" || frame < 0 {
		return ""
	}
	return prefixSSE + strconv.Itoa(frame) + ":" + path
}

// Multipart addresses a named part of a multipart body.
func Multipart(part string) string {
	if part == "" {
		return ""
	}
	return prefixMultipart + part
}

// -------------------------------------------------------------- predicates

// TrimIgnorable strips leading and trailing characters that carry no meaning
// in a URI: whitespace, and the Unicode format category.
//
// The format half is not decoration. U+FEFF, U+200B and U+200E are category
// Cf and not White_Space, so a whitespace-only trim let a whole data URI
// walk past a scheme test and land in a reference field. Expressed as
// categories rather than an enumeration because the enumerated version
// covered 32 code points against this set's 195.
func TrimIgnorable(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.Is(unicode.Cf, r)
	})
}

// HasSchemeFold reports whether s begins with the given URI scheme,
// case-insensitively. RFC 3986 §3.1 makes schemes case-insensitive, so
// `DATA:image/png;base64,…` is the same URI as `data:…`: a case-sensitive
// test does not reject an attack, it misreads a legal spelling.
func HasSchemeFold(s, scheme string) bool {
	if len(s) < len(scheme) {
		return false
	}
	return strings.EqualFold(s[:len(scheme)], scheme)
}

// ParseDataURI splits a data URI into its declared mime and its payload.
// The mime is a display hint only — the bytes decide what the artifact
// actually is.
func ParseDataURI(s string) (mimeType, payload string, ok bool) {
	if !HasSchemeFold(s, "data:") {
		return "", "", false
	}
	comma := strings.IndexByte(s, ',')
	if comma < 0 {
		return "", "", false
	}
	meta := s[len("data:"):comma]
	if !strings.Contains(meta, "base64") {
		return "", "", false
	}
	mimeType = strings.TrimSuffix(meta, ";base64")
	mimeType = strings.TrimSpace(strings.TrimSuffix(mimeType, ";"))
	return mimeType, s[comma+1:], true
}

// ValidBase64 reports whether s decodes under base64.StdEncoding.
//
// This is the predicate that decides whether a codec may call bytes
// "captured", and Resolve below decodes with exactly the encoding it
// describes. One predicate, both callers: a payload this accepts is a
// payload the resolver can serve, which is what makes "the card offered a
// download" and "the endpoint can serve it" impossible to disagree about.
//
// Padding is therefore required and the alphabet is the standard one only.
// Rejecting an unpadded or url-safe payload is the safe error, and no
// provider on any measured wire sends one.
func ValidBase64(s string) bool {
	// StdEncoding ignores CR and LF, so this must too, or the predicate is
	// stricter than the decoder it claims to describe: a line-wrapped
	// payload that Resolve can serve perfectly well would be reported
	// unreachable. Measured — DecodeString("QUJD\n") succeeds.
	s = stripLineBreaks(s)
	if len(s) == 0 || len(s)%4 != 0 {
		return false
	}
	body := s
	pad := 0
	for pad < 2 && len(body) > 0 && body[len(body)-1] == '=' {
		body = body[:len(body)-1]
		pad++
	}
	for i := range len(body) {
		c := body[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '+' || c == '/':
		default:
			return false
		}
	}
	return true
}

// DecodedSize returns the byte length a base64 payload decodes to, by
// arithmetic. It never decodes: sizing a 20 MB payload must not allocate
// 15 MB to answer a question about a number.
func DecodedSize(b64 string) int64 {
	// Same line-break tolerance as ValidBase64 and the decoder, or a
	// wrapped payload reports a size larger than the bytes it yields.
	s := strings.TrimRight(stripLineBreaks(b64), "=")
	n := len(s)
	if n == 0 {
		return 0
	}
	full := n / 4
	size := int64(full) * 3
	switch n % 4 {
	case 2:
		size++
	case 3:
		size += 2
	}
	return size
}

// stripLineBreaks removes the two characters base64.StdEncoding ignores.
// Nothing else: a space or a zero-width character is NOT ignored by the
// decoder, so treating it as ignorable here would re-open the gap this
// exists to close.
func stripLineBreaks(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := range len(s) {
		if c := s[i]; c != '\r' && c != '\n' {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// --------------------------------------------------------------- resolving

// Resolved is the outcome of a successful resolution.
type Resolved struct {
	// Bytes are the artifact, decoded.
	Bytes []byte
	// Mime is what the CONTAINER declared, when it declared anything — a
	// data URI's meta, or a multipart part's Content-Type. It is a hint for
	// naming a download, never the served Content-Type: the caller sniffs
	// the bytes for that, because a declared type is attacker-controlled.
	Mime string
}

// Resolve reads the bytes a locator addresses out of the stored body of the
// same direction as the payload that carried the reference.
func Resolve(body []byte, loc string) (Resolved, error) {
	switch {
	case loc == "":
		return Resolved{}, failf(ReasonMalformed, "empty locator")

	case loc == Body:
		if len(body) == 0 {
			return Resolved{}, failf(ReasonNotFound, "stored body is empty")
		}
		return Resolved{Bytes: body}, nil

	case strings.HasPrefix(loc, prefixJSON):
		v, err := walk(body, strings.TrimPrefix(loc, prefixJSON))
		if err != nil {
			return Resolved{}, err
		}
		return decode(v, "")

	case strings.HasPrefix(loc, prefixDataURI):
		v, err := walk(body, strings.TrimPrefix(loc, prefixDataURI))
		if err != nil {
			return Resolved{}, err
		}
		mimeType, payload, ok := ParseDataURI(TrimIgnorable(v))
		if !ok {
			return Resolved{}, failf(ReasonUndecodable, "value at %q is not a base64 data URI", loc)
		}
		return decode(payload, mimeType)

	case strings.HasPrefix(loc, prefixSSE):
		rest := strings.TrimPrefix(loc, prefixSSE)
		colon := strings.IndexByte(rest, ':')
		if colon < 0 {
			return Resolved{}, failf(ReasonMalformed, "sse locator carries no path")
		}
		frame, err := strconv.Atoi(rest[:colon])
		if err != nil || frame < 0 {
			return Resolved{}, failf(ReasonMalformed, "sse frame index %q is not a number", rest[:colon])
		}
		frames := DataFrames(body)
		if frame >= len(frames) {
			return Resolved{}, failf(ReasonNotFound, "sse frame %d of %d", frame, len(frames))
		}
		v, err := walk(frames[frame], rest[colon+1:])
		if err != nil {
			return Resolved{}, err
		}
		// A streamed part may carry either shape, so try the richer one
		// first and fall back rather than making the codec declare which.
		if mimeType, payload, ok := ParseDataURI(TrimIgnorable(v)); ok {
			return decode(payload, mimeType)
		}
		return decode(v, "")

	case strings.HasPrefix(loc, prefixMultipart):
		return resolveMultipart(body, strings.TrimPrefix(loc, prefixMultipart))
	}
	return Resolved{}, failf(ReasonUnsupported, "unknown container in %q", loc)
}

func walk(doc []byte, path string) (string, error) {
	if path == "" {
		return "", failf(ReasonMalformed, "empty path")
	}
	r := gjson.GetBytes(doc, path)
	if !r.Exists() {
		return "", failf(ReasonNotFound, "no value at %q", path)
	}
	if r.Type != gjson.String {
		return "", failf(ReasonUndecodable, "value at %q is %s, not a string", path, r.Type)
	}
	return r.String(), nil
}

// decode turns a base64 payload into bytes under the same predicate the
// codecs use to promise it is decodable.
func decode(payload, mimeType string) (Resolved, error) {
	payload = TrimIgnorable(payload)
	if payload == "" {
		return Resolved{}, failf(ReasonNotFound, "payload is empty")
	}
	if !ValidBase64(payload) {
		return Resolved{}, failf(ReasonUndecodable, "payload is not standard base64")
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		// Unreachable while ValidBase64 and StdEncoding agree; kept because
		// the day they stop agreeing is the day this must not serve garbage.
		return Resolved{}, failf(ReasonUndecodable, "payload failed to decode: %v", err)
	}
	return Resolved{Bytes: raw, Mime: mimeType}, nil
}

// DataFrames returns the payloads of the data-bearing frames of an SSE body,
// in arrival order — the order sse: locator indices count in.
//
// Exported because the same walk decides both "which frame is index N" when
// resolving and "what index am I in" when a codec folds a stream. Two
// implementations of frame counting is an off-by-one waiting to happen.
func DataFrames(body []byte) [][]byte {
	var out [][]byte
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

	var data []string
	dispatch := func() {
		if len(data) == 0 {
			return
		}
		// One optional space after the colon is grammar, not payload;
		// multi-line frames concatenate, which is how a producer splits a
		// long JSON document across lines.
		s := strings.Join(data, "")
		data = data[:0]
		if s == "" || s == "[DONE]" {
			return
		}
		out = append(out, []byte(s))
	}

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		// A frame ends at a blank line. bufio.Scanner is what makes this
		// CRLF-correct — ScanLines strips a terminal CR — and replacing it
		// with a literal strings.Split(body, "\n\n") is what merged two
		// CRLF frames into one. TrimSpace here is belt-and-braces and
		// cannot be falsified on its own; the Scanner is the load-bearing
		// part, and the sibling walkSSEFrames is written the same way. The
		// bug this closes:
		// the index a
		// codec emitted and the index this counted disagreed, so a captured
		// ref pointed at a frame that did not exist and its Download 404'd.
		// The codec's own fold and the browser resolver were both already
		// CRLF-correct; this walker was the odd one out.
		case strings.TrimSpace(line) == "":
			dispatch()
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimSuffix(line[len("data:"):], "\r"), " "))
		}
	}
	dispatch()
	return out
}

// resolveMultipart finds a named part of a multipart body.
//
// The part is addressed by form field name, which is what the wire calls it
// (`file`, `image`), rather than by index: an index would change meaning if
// a client reordered its fields, and the whole point of a locator is that
// it means the same thing every time it is read.
func resolveMultipart(body []byte, part string) (Resolved, error) {
	if part == "" {
		return Resolved{}, failf(ReasonMalformed, "multipart locator names no part")
	}
	boundary, err := sniffBoundary(body)
	if err != nil {
		return Resolved{}, err
	}
	r := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		p, err := r.NextPart()
		if err != nil {
			// io.EOF or a malformed tail: either way the part is not here.
			return Resolved{}, failf(ReasonNotFound, "no multipart part named %q", part)
		}
		if p.FormName() != part {
			continue
		}
		raw, err := io.ReadAll(p)
		if err != nil {
			return Resolved{}, failf(ReasonUndecodable, "part %q could not be read: %v", part, err)
		}
		declared := p.Header.Get("Content-Type")
		if mt, _, err := mime.ParseMediaType(declared); err == nil {
			declared = mt
		}
		return Resolved{Bytes: raw, Mime: declared}, nil
	}
}

// sniffBoundary recovers the multipart boundary from the body itself.
//
// The stored body is all there is — the request's Content-Type header is
// not part of it — so the boundary is read from the first delimiter line.
// That is exactly as reliable as the body being a multipart body at all,
// which the locator already asserts.
func sniffBoundary(body []byte) (string, error) {
	s := string(body)
	end := strings.IndexAny(s, "\r\n")
	if end < 0 || !strings.HasPrefix(s, "--") {
		return "", failf(ReasonUndecodable, "stored body does not open with a multipart delimiter")
	}
	b := strings.TrimSpace(s[2:end])
	if b == "" {
		return "", failf(ReasonUndecodable, "multipart delimiter names no boundary")
	}
	return b, nil
}
