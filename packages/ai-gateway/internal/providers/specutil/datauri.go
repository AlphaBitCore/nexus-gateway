package specutil

import (
	"encoding/base64"
	"strings"
)

// ParseDataURL NORMALIZES to standard padded base64 rather than merely
// validating. The same PNG sent directly to two vendors in four encodings:
//
//	                  api.anthropic.com   generativelanguage.googleapis.com
//	padded base64     200                 200, answered from the image
//	unpadded          400 invalid base64  200, answered from the image
//	base64url         (not probed)        200, answered from the image
//	newline-wrapped   —                   400 Invalid value
//	corrupt           400                 400
//
// Validating with StdEncoding alone would refuse two forms Google decodes;
// passing the payload through has the opposite fault, since Go's decoder
// ignores \r and \n. A blank media type defaults to application/octet-stream
// per the data: scheme.
func ParseDataURL(dataURL string) (mediaType, b64 string, ok bool) {
	if !strings.HasPrefix(dataURL, "data:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(dataURL, "data:")
	comma := strings.Index(rest, ",")
	if comma < 0 || comma == len(rest)-1 {
		return "", "", false
	}
	meta, payload := rest[:comma], rest[comma+1:]
	if !strings.HasSuffix(meta, ";base64") {
		return "", "", false
	}
	mediaType = normalizeMediaType(strings.TrimSuffix(meta, ";base64"))
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	raw, ok := decodeAnyBase64(payload)
	if !ok {
		return "", "", false
	}
	return mediaType, base64.StdEncoding.EncodeToString(raw), true
}

// Measured against api.anthropic.com with the same PNG: "image/png" answers 200
// while " image/png ", "image/png; charset=utf-8", "IMAGE/PNG" and "Image/Png"
// all answer 400 naming the enum — RFC 2045 makes a media type
// case-INSENSITIVE, so an uppercase spelling is the same type by the standard
// and a different string to the vendor.
func normalizeMediaType(mt string) string {
	if i := strings.Index(mt, ";"); i >= 0 {
		mt = mt[:i]
	}
	return strings.ToLower(strings.TrimSpace(mt))
}

// Shared so Cohere (no binary document at all) and Anthropic (source shape
// depends on it) cannot drift into one reading a .yaml the other refused.
// Declared type only — a small PDF is printable enough to fool sniffing.
func IsTextualMediaType(mediaType string) bool {
	mt := normalizeMediaType(mediaType)
	if strings.HasPrefix(mt, "text/") {
		return true
	}
	switch mt {
	case "application/json", "application/xml", "application/yaml",
		"application/x-yaml", "application/toml", "application/csv",
		"application/x-ndjson":
		return true
	}
	// Structured suffixes: application/vnd.api+json and friends are text.
	return strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "+xml") ||
		strings.HasSuffix(mt, "+yaml")
}

// Standard or URL alphabet, padded or not. Whitespace is stripped rather than
// tolerated: Go's decoders skip \r and \n, which is how a line-wrapped payload
// reaches a vendor that refuses it.
func decodeAnyBase64(payload string) ([]byte, bool) {
	clean := strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', ' ', '\t':
			return -1
		}
		return r
	}, payload)
	if clean == "" {
		return nil, false
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if raw, err := enc.DecodeString(clean); err == nil {
			return raw, true
		}
	}
	return nil, false
}
