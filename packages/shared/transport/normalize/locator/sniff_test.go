package locator

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type sniffVector struct {
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
	Mime   string `json:"mime"`
	Ext    string `json:"ext"`
}

func loadSniffVectors(t *testing.T) []sniffVector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "sniff-vectors.json"))
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var doc struct {
		Vectors []sniffVector `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	if len(doc.Vectors) < 20 {
		t.Fatalf("the table has shrunk to %d vectors — it is the only thing keeping the two sniffers in step", len(doc.Vectors))
	}
	return doc.Vectors
}

// The Go sniffer against the shared table. The browser's sniffer asserts
// against the same file; that is what makes it impossible for the two to
// answer differently about the same bytes without a test going red.
func TestSniffMimeMatchesTheSharedVectorTable(t *testing.T) {
	for _, v := range loadSniffVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			b, err := hex.DecodeString(v.Prefix)
			if err != nil {
				t.Fatalf("vector %q has a bad prefix: %v", v.Name, err)
			}
			if got := SniffMime(b); got != v.Mime {
				t.Fatalf("SniffMime = %q, table says %q", got, v.Mime)
			}
			if got := ExtensionForMime(v.Mime); got != v.Ext {
				t.Fatalf("ExtensionForMime(%q) = %q, table says %q", v.Mime, got, v.Ext)
			}
		})
	}
}

// The brands that matter most, called out separately because getting them
// wrong is not a cosmetic mislabel: an iPhone photo served as video/mp4 is
// offered inline, named .mp4, and opens as nothing.
func TestISOBMFFBrandsAreNotAllVideo(t *testing.T) {
	brand := func(s string) []byte {
		return append([]byte{0, 0, 0, 0x1c}, append([]byte("ftyp"), []byte(s)...)...)
	}
	for s, want := range map[string]string{
		"heic": "image/heic", "heix": "image/heic",
		"mif1": "image/heif", "msf1": "image/heif",
		"avif": "image/avif", "avis": "image/avif",
		"M4A ": "audio/mp4",
		"isom": "video/mp4", "mp42": "video/mp4", "qt  ": "video/mp4",
	} {
		if got := SniffMime(brand(s)); got != want {
			t.Fatalf("brand %q -> %q, want %q", s, got, want)
		}
	}
	// A truncated box names the container it is, not a guess about content.
	if got := SniffMime([]byte{0, 0, 0, 0x1c, 'f', 't', 'y', 'p'}); got != "video/mp4" {
		t.Fatalf("truncated ftyp -> %q", got)
	}
}

// Anything the sniffer can return must have an extension, or a correctly
// identified artifact still downloads as a nameless .bin.
func TestEverySniffableTypeHasAnExtension(t *testing.T) {
	for _, m := range []string{
		"image/png", "image/jpeg", "image/gif", "image/webp",
		"image/heic", "image/heif", "image/avif",
		"audio/wav", "audio/mpeg", "audio/flac", "audio/ogg", "audio/mp4",
		"video/mp4", "video/webm", "application/pdf",
	} {
		if ExtensionForMime(m) == "bin" {
			t.Fatalf("%s is sniffable but downloads as .bin", m)
		}
	}
	// A parameterised type still resolves.
	if got := ExtensionForMime("audio/mpeg; charset=binary"); got != "mp3" {
		t.Fatalf("parameterised type -> %q", got)
	}
	if got := ExtensionForMime("application/octet-stream"); got != "bin" {
		t.Fatalf("the unknown type is .bin, got %q", got)
	}
}
