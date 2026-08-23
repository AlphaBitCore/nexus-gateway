package locator

import "strings"

// SniffMime returns the media type of a byte sequence from its signature,
// and application/octet-stream when it recognises nothing.
//
// The bytes decide, never the declared type: a data URI's meta, a multipart
// part's Content-Type and a provider's `mimeType` field are all written by
// whoever sent the payload, so trusting one is how a script arrives labelled
// as a picture. The declared value survives on the ref as a display hint and
// nothing more.
//
// This is the ONE sniffer on the Go side. The browser has its own — the two
// cannot share code across the language boundary — so they share a vector
// table instead (testdata/sniff-vectors.json), and both assert against it.
// Before that table they disagreed: an iPhone photo submitted as a video
// input reference was served inline as video/mp4 and downloaded as .mp4 by
// the server, while the Agent Dashboard called the same bytes image/heic.
func SniffMime(b []byte) string {
	switch {
	case has(b, "\x89PNG\r\n\x1a\n"):
		return "image/png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case has(b, "GIF87a"), has(b, "GIF89a"):
		return "image/gif"
	case has(b, "RIFF") && hasAt(b, 8, "WEBP"):
		return "image/webp"
	case has(b, "RIFF") && hasAt(b, 8, "WAVE"):
		return "audio/wav"
	case hasAt(b, 4, "ftyp"):
		return isobmff(b)
	case has(b, "ID3"):
		return "audio/mpeg"
	case len(b) >= 2 && b[0] == 0xFF && (b[1]&0xE0) == 0xE0:
		return "audio/mpeg" // MPEG/ADTS frame sync
	case has(b, "fLaC"):
		return "audio/flac"
	case has(b, "OggS"):
		return "audio/ogg"
	case has(b, "%PDF-"):
		return "application/pdf"
	case has(b, "\x1aE\xdf\xa3"):
		// EBML. Every Matroska variant a provider could return is served the
		// same way, and the DocType that would separate webm from generic
		// mkv can sit past any window worth scanning — so the container is
		// named by its magic and the distinction is not attempted.
		return "video/webm"
	}
	return "application/octet-stream"
}

// isobmff separates the ISO base-media brands. The same container carries
// still images, audio and video, so the brand is the only thing that says
// which — and reading it wrong is what served a photo as a movie.
func isobmff(b []byte) string {
	if len(b) < 12 {
		return "video/mp4"
	}
	switch string(b[8:12]) {
	case "heic", "heix", "hevc", "hevx":
		return "image/heic"
	case "mif1", "msf1":
		return "image/heif"
	case "avif", "avis":
		return "image/avif"
	case "M4A ", "M4B ", "M4P ":
		return "audio/mp4"
	}
	return "video/mp4"
}

func has(b []byte, sig string) bool { return hasAt(b, 0, sig) }

func hasAt(b []byte, at int, sig string) bool {
	return len(b) >= at+len(sig) && string(b[at:at+len(sig)]) == sig
}

// ExtensionForMime names a download so the reader's system can open it.
// Kept beside the sniffer because the two answer one question together, and
// a type the sniffer can return with no extension here is a file that
// downloads as .bin for no reason.
func ExtensionForMime(mime string) string {
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	switch mime {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	case "image/heic":
		return "heic"
	case "image/heif":
		return "heif"
	case "image/avif":
		return "avif"
	case "audio/wav":
		return "wav"
	case "audio/mpeg":
		return "mp3"
	case "audio/flac":
		return "flac"
	case "audio/ogg":
		return "ogg"
	case "audio/mp4":
		return "m4a"
	case "audio/webm":
		return "weba"
	case "audio/aac":
		return "aac"
	case "video/mp4":
		return "mp4"
	case "video/webm":
		return "webm"
	case "application/pdf":
		return "pdf"
	}
	return "bin"
}
