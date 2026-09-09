package specutil

import (
	"path"
	"strings"
)

// FilenameForMime supplies the name of a file part whose wire had nowhere to
// keep one.
//
// OpenAI rejects a file part carrying `file_data` without `filename`
// ("Missing required parameter: messages[N].content[M].file.filename"), and
// several wires — Gemini's inlineData among them — carry the bytes and the mime
// and nothing else. A request that was valid when it arrived would otherwise
// come back from such a hop as a 400.
//
// This is the same auto-fill the Anthropic max_tokens default performs: a
// protocol-required field is filled from what the wire DOES state rather than
// failing the caller for a field the intermediate format could not carry. It is
// not a claim to have preserved the caller's name — that name is genuinely lost
// on a wire with no slot for it, and saying so plainly is better than inventing
// a carrier no provider reads.
//
// The stem is constant on purpose. Deriving it from the content would put caller
// bytes into a filename, and deriving it from a counter would make the same
// request encode differently on every attempt, which breaks cache keys.
func FilenameForMime(mime string) string {
	ext := extensionForMime(mime)
	return "attachment" + ext
}

// extensionForMime maps the mime types that actually reach a file part to a
// conventional extension. Anything unrecognised gets none: an extension that
// contradicts the bytes is worse than no extension, because a reader trusts it.
func extensionForMime(mime string) string {
	switch strings.ToLower(strings.TrimSpace(path.Base(mime))) {
	case "pdf", "x-pdf":
		return ".pdf"
	case "plain":
		return ".txt"
	case "markdown", "x-markdown":
		return ".md"
	case "csv":
		return ".csv"
	case "json":
		return ".json"
	case "html":
		return ".html"
	case "xml":
		return ".xml"
	case "msword":
		return ".doc"
	case "vnd.openxmlformats-officedocument.wordprocessingml.document":
		return ".docx"
	case "vnd.ms-excel":
		return ".xls"
	case "vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return ".xlsx"
	}
	return ""
}
