package cohere

// A canonical `file` part forwarded verbatim to /v2/chat comes back 422
// "unrecognized content type 'file'"; Cohere carries documents in a TOP-LEVEL
// `documents` array instead. That array holds TEXT passages with an optional
// title, not binary — so a PDF has no shape here and is refused rather than
// extracted, which would change what the model is given.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
)

func errContentUnsupported(detail string) error {
	return &provcore.ProviderError{
		Status:  http.StatusBadRequest,
		Code:    provcore.CodeInvalidRequest,
		Type:    "nexus_field_unsupported",
		Message: "nexus: " + detail,
	}
}

// Scanned before parsing: a body carrying neither token pays one substring
// search each and no parse, which is nearly every request.
var fileTypeToken = []byte(`"file"`)

var imageURLToken = []byte(`"image_url"`)

// Shared with Anthropic, which needs the same fact to choose between a base64
// and a text document source; two copies would drift.
func isDocumentTextMediaType(mediaType string) bool {
	return specutil.IsTextualMediaType(mediaType)
}

// A `file` part is LIFTED onto the top-level `documents` array and removed from
// the message it rode in on; an `image_url` part that is not a data: URL is
// refused, since this wire reads images from inline bytes and does not fetch.
func translateContentForCohereChat(body []byte) ([]byte, error) {
	if !bytes.Contains(body, fileTypeToken) && !bytes.Contains(body, imageURLToken) {
		return body, nil
	}
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return body, nil
	}

	type lifted struct {
		title string
		text  string
	}
	var docs []lifted
	var removals []string
	var firstErr error

	mi := -1
	msgs.ForEach(func(_, msg gjson.Result) bool {
		mi++
		content := msg.Get("content")
		if !content.IsArray() {
			return true
		}
		pi, files, total := -1, 0, 0
		content.ForEach(func(_, part gjson.Result) bool {
			pi++
			total++
			if part.Get("type").String() == "image_url" {
				if u := part.Get("image_url.url").String(); u != "" && !strings.HasPrefix(u, "data:") {
					// Measured against api.cohere.com with the same PNG: a
					// data: URL answers 200, a reachable https URL answers
					// 400 "invalid request: Invalid url '<url>'" — a
					// categorical refusal wearing the words of a bad URL.
					firstErr = errContentUnsupported(
						"Cohere chat reads images from inline bytes only, so an image_url must " +
							"be a data: URL carrying the image (data:image/png;base64,…); this " +
							"wire does not fetch images by URL")
					return false
				}
				return true
			}
			if part.Get("type").String() != "file" {
				return true
			}
			doc, err := filePartToDocument(part.Get("file"))
			if err != nil {
				firstErr = err
				return false
			}
			files++
			docs = append(docs, doc)
			removals = append(removals, fmt.Sprintf("messages.%d.content.%d", mi, pi))
			return true
		})
		if firstErr != nil {
			return false
		}
		if files > 0 && files == total {
			// Measured: Cohere answers 400 "missing required parameter" for an
			// empty content array and 400 "'text' field must not be empty" for
			// an empty string — no shape carries a document alone.
			firstErr = errContentUnsupported(fmt.Sprintf(
				"messages.%d carries only a file and no text; Cohere chat needs a question "+
					"alongside a document", mi))
			return false
		}
		return true
	})
	if firstErr != nil {
		return nil, firstErr
	}
	if len(docs) == 0 {
		return body, nil
	}

	// Unwind from the end: deleting shifts the indices of everything after it.
	// sjson errors only on an unparseable PATH, and these are counted indices.
	out := body
	for i := len(removals) - 1; i >= 0; i-- {
		out, _ = sjson.DeleteBytes(out, removals[i])
	}

	// Ids are taken against the ids actually present, not numbered from the
	// array's LENGTH: a caller's ids are arbitrary strings, so a caller document
	// id'd "doc-1" would collide. Cohere's citations reference documents by id,
	// so a collision misattributes a cited span — with a 200.
	existing := gjson.GetBytes(out, "documents")
	if existing.Exists() && !existing.IsArray() {
		// sjson's "documents.-1" appends to an array; against anything else it
		// writes a literal "-1" KEY, silently destroying what the caller put there.
		return nil, errContentUnsupported(
			"documents must be an array to carry a file part alongside it")
	}
	taken := make(map[string]struct{}, len(existing.Array()))
	existing.ForEach(func(_, d gjson.Result) bool {
		taken[d.Get("id").String()] = struct{}{}
		return true
	})

	next := 0
	for _, d := range docs {
		var id string
		for {
			id = fmt.Sprintf("doc-%d", next)
			next++
			if _, clash := taken[id]; !clash {
				break
			}
		}
		taken[id] = struct{}{}
		entry := map[string]any{"id": id}
		data := map[string]any{"text": d.text}
		if d.title != "" {
			data["title"] = d.title
		}
		entry["data"] = data
		out, _ = sjson.SetBytes(out, "documents.-1", entry)
	}
	return out, nil
}

func filePartToDocument(file gjson.Result) (struct {
	title string
	text  string
}, error) {
	var doc struct {
		title string
		text  string
	}
	name := file.Get("filename").String()

	// file_data is switched on PRESENCE, not on the data: prefix: a present but
	// non-data: file_data would otherwise fall to the default arm and be told it
	// sent no file_data at all.
	switch {
	case file.Get("file_data").Exists():
		declared := file.Get("file_data").String()
		if !strings.HasPrefix(declared, "data:") {
			return doc, errContentUnsupported(
				"file.file_data must be a data: URL carrying the document's bytes " +
					"(data:text/plain;base64,…); a bare string or an http URL is not read")
		}
		mediaType, b64, ok := specutil.ParseDataURL(declared)
		if !ok {
			return doc, errContentUnsupported("file.file_data is not a decodable data: URL")
		}
		if !isDocumentTextMediaType(mediaType) {
			return doc, errContentUnsupported(fmt.Sprintf(
				"Cohere chat carries a file part as a text passage in its documents array, "+
					"and %s is not text. Extracting it would change what the model is given, "+
					"so send the document's text instead — as a text/* file part or in the "+
					"message", mediaType))
		}
		// ParseDataURL returns ok only for a payload it already decoded.
		raw, _ := base64.StdEncoding.DecodeString(b64)
		if !utf8.Valid(raw) {
			return doc, errContentUnsupported(fmt.Sprintf(
				"file.file_data declares %s but the bytes are not valid UTF-8", mediaType))
		}
		doc.text = string(raw)
	case file.Get("file_id").Exists():
		return doc, errContentUnsupported(
			"Cohere chat has no files API to resolve file_id against; send the document's " +
				"text as a text/* file part instead")
	case file.Get("file_url").Exists():
		return doc, errContentUnsupported(
			"Cohere chat does not fetch documents by URL; send the document's text as a " +
				"text/* file part instead")
	default:
		return doc, errContentUnsupported("file part carries no file_data, file_id or file_url")
	}

	doc.title = name
	return doc, nil
}
