package aiguard

import (
	"crypto/sha256"
	"encoding/hex"
)

// BackendFingerprint returns a 64-char lowercase hex sha256 over the
// tuple that uniquely identifies a judge configuration:
//
//	mode | providerID_or_URL | model | promptTemplateSHA
//
// Partitions the cache on any material backend change so switching
// provider / model / prompt cleanly ages out old entries via TTL.
func BackendFingerprint(mode, providerOrURL, model, promptTemplateSHA string) string {
	h := sha256.New()
	h.Write([]byte(mode))
	h.Write([]byte{'|'})
	h.Write([]byte(providerOrURL))
	h.Write([]byte{'|'})
	h.Write([]byte(model))
	h.Write([]byte{'|'})
	h.Write([]byte(promptTemplateSHA))
	return hex.EncodeToString(h.Sum(nil))
}

// PromptTemplateSHA returns the lowercase hex sha256 of the prompt template.
// Stored alongside BackendFingerprint so admins can see which template was
// in effect for a given cached judgment.
func PromptTemplateSHA(template string) string {
	sum := sha256.Sum256([]byte(template))
	return hex.EncodeToString(sum[:])
}
