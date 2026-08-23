// Media is one concept with one shape: a reference that names WHAT the bytes
// are, PROVES which bytes they were, and — when they are recoverable — says
// where to find them. It never carries content in any encoding.
//
// Split out of types.go because it had grown into a subject of its own, and
// the file-size ratchet is the repo's way of noticing that before a file
// becomes the place everything goes.

package core

// MediaRef describes one media element of a captured exchange. It never
// carries content bytes in any encoding: the bytes already live exactly
// once inside the captured, redaction-governed body, and Locator makes
// them addressable there. Exactly one of Locator / URL / ProviderRef is
// set, matching Source; every other Source sets none of them.
type MediaRef struct {
	// Modality is "image", "audio", "video" or "file".
	Modality string `json:"modality"`
	// Mime is the wire-declared type (data-URI prefix, media_type,
	// mimeType). It is a display hint ONLY — never use it to construct a
	// Blob type, because the wire declares it and a caller controls the
	// wire. Empty means unknown.
	Mime string `json:"mime,omitempty"`
	// SizeBytes is the decoded size, computed arithmetically from the
	// base64 length. Normalizing never decodes to learn it.
	//
	// Deliberately NOT omitempty: a genuinely empty element and an element
	// whose size is unknown are different facts, and collapsing them would
	// make a zero-byte payload indistinguishable from a missing one — the
	// exact ambiguity that let a size of 0 read as "fine" during the
	// investigation.
	SizeBytes int64 `json:"sizeBytes"`
	// Source is the custody state: captured, external, provider-ref,
	// fingerprint, aged-out, redacted or absent. Only "captured" has
	// reachable bytes, and it is the only one a download is offered for.
	Source string `json:"source"`
	// Locator addresses the bytes inside the captured body of the same
	// direction. Set only when Source is "captured".
	Locator string `json:"locator,omitempty"`
	// URL is an inert remote reference the system never fetches.
	URL string `json:"url,omitempty"`
	// ProviderRef is a provider-held file id or file URI.
	ProviderRef string `json:"providerRef,omitempty"`
	// SHA256 is a real hex digest or empty — never a payload prefix.
	SHA256 string `json:"sha256,omitempty"`
	// Truncated marks bytes as unrecoverable: capture cut before this
	// element fully landed, or the captured element fails the decode
	// validator. Either way no download is offered.
	Truncated bool `json:"truncated,omitempty"`
	// Cause names, machine-readably, why bytes are unreachable despite
	// evidence they existed — the classified spill failure for aged-out,
	// the validator failure class for truncated.
	Cause string `json:"cause,omitempty"`
}

// Media custody states.
//
// Six, and every one has a producer. A seventh, "redacted", was defined
// alongside these and nothing ever emitted it: compliance redacts text, not
// media. A custody value nothing can reach is a distinction the system
// claims to make and does not, so it is gone until media redaction exists
// and can set it.
//
// MediaCaptured is the only state with reachable bytes, and even it yields
// them only while Truncated is false — the pair is what mediaHasBytes reads,
// and what decides whether a card offers a control at all.
const (
	MediaCaptured    = "captured"
	MediaExternal    = "external"
	MediaProviderRef = "provider-ref"
	MediaFingerprint = "fingerprint"
	MediaAgedOut     = "aged-out"
	MediaAbsent      = "absent"
)

// Media modalities.
const (
	ModalityImage = "image"
	ModalityAudio = "audio"
	ModalityVideo = "video"
	ModalityFile  = "file"
)
